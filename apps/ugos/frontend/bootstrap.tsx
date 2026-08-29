import { createApp } from "@shared/app/createApp";
import { ugosBridge } from "./platform";
import "./provider.css";

export default function bootstrap(container: HTMLElement) {
  createApp(container, ugosBridge);
}
