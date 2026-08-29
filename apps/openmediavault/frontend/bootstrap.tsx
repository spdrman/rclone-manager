import { createApp } from "@shared/app/createApp";
import { openmediavaultBridge } from "./platform";

export default function bootstrap(container: HTMLElement) {
  createApp(container, openmediavaultBridge);
}
