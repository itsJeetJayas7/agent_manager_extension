import React from "react";
import { createRoot } from "react-dom/client";
import App from "./App";
import { announceReady } from "./api/client";
import "./styles/global.css";

const mode = document.body.dataset.mode ?? "sidebar";

const root = createRoot(document.getElementById("root")!);
root.render(
  <React.StrictMode>
    <App mode={mode as "sidebar" | "planner" | "detail"} />
  </React.StrictMode>
);

// Tell the host we exist. Anything it posted before this point was queued;
// without the handshake those messages (e.g. detailBatchId) were dropped.
announceReady();
