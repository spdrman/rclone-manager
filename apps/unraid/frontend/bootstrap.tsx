import { createApp } from "@shared/app/createApp";
import { unraidBridge } from "./platform";

export default function bootstrap(container: HTMLElement) {
  createApp(container, unraidBridge);
}
