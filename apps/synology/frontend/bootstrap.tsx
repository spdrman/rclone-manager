import { createApp } from "@shared/app/createApp";
import { synologyBridge } from "./platform";
import "./provider.css";

export default function bootstrap(container: HTMLElement) {
  createApp(container, synologyBridge);
}
