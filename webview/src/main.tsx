import React from "react";
import { createRoot } from "react-dom/client";
import App from "./App";
import "./styles/global.css";

const mode = document.body.dataset.mode ?? "sidebar";

const root = createRoot(document.getElementById("root")!);
root.render(
  <React.StrictMode>
    <App mode={mode as "sidebar" | "planner" | "detail"} />
  </React.StrictMode>
);
