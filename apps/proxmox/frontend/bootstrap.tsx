import { createApp } from "@shared/app/createApp";
import { proxmoxBridge } from "./platform";

export default function bootstrap(container: HTMLElement) {
  createApp(container, proxmoxBridge);
}
