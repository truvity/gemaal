import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

// Built assets are embedded into the Go binary and served at the root.
export default defineConfig({ base: "/", plugins: [react()] });
