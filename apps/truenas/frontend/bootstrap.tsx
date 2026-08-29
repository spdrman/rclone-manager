import { createApp } from "@shared/app/createApp";
import { truenasBridge } from "./platform";

export default function bootstrap(container: HTMLElement) {
  createApp(container, truenasBridge);
}
