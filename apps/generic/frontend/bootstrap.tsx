import { createApp } from "@shared/app/createApp";
import { genericBridge } from "./platform";
import "./provider.css";

export default function bootstrap(container: HTMLElement) {
  createApp(container, genericBridge);
}
