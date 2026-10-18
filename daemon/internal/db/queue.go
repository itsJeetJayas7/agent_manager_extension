package db

import "fmt"

// ReorderQueue changes only queue positions in one transaction. Active runs
// cannot be moved, and concurrent state updates cannot be overwritten by an
// outdated Run object. Every waiting state participates, including quota waits.
func (s *Store) ReorderQueue(runID, beforeRunID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ph, args := stateArgs(queueOrderStates)
	rows, err := tx.Query(`SELECT r.id, r.batch_id, br.position FROM runs r
		JOIN batch_rows br ON br.id=r.row_id WHERE r.state IN (`+ph+`)
		ORDER BY r.sort_key, r.created_at, r.id`, args...)
	if err != nil {
		return err
	}
	type entry struct {
		id, batch string
		row       int
	}
	var order []entry
	from := -1
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.id, &e.batch, &e.row); err != nil {
			rows.Close()
			return err
		}
		if e.id == runID {
			from = len(order)
		}
		order = append(order, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if from < 0 {
		return fmt.Errorf("only waiting runs can be reordered; this run may have already started")
	}
	if runID == beforeRunID {
		return nil
	}
	moving := order[from]
	order = append(order[:from], order[from+1:]...)
	to := len(order)
	if beforeRunID != "" {
		to = -1
		for i, e := range order {
			if e.id == beforeRunID {
				to = i
				break
			}
		}
		if to < 0 {
			return fmt.Errorf("target must still be waiting; refresh the queue and try again")
		}
	}
	// Check siblings on both sides, even when the drop target is in another
	// batch. Sequential prompt steps must keep their dependency order.
	for i, e := range order {
		if e.batch == moving.batch && ((i < to && e.row > moving.row) || (i >= to && e.row < moving.row)) {
			return fmt.Errorf("cannot move a run across its batch row dependency")
		}
	}
	order = append(order, entry{})
	copy(order[to+1:], order[to:])
	order[to] = moving
	for i, e := range order {
		if _, err := tx.Exec(`UPDATE runs SET sort_key=? WHERE id=?`, int64(i+1)*SortKeyGap, e.id); err != nil {
			return err
		}
	}
	return tx.Commit()
}
