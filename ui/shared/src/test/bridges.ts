import { genericBridge } from "../../../../apps/generic/frontend/platform";
import { ugosBridge } from "../../../../apps/ugos/frontend/platform";
import { synologyBridge } from "../../../../apps/synology/frontend/platform";
import { truenasBridge } from "../../../../apps/truenas/frontend/platform";
import { unraidBridge } from "../../../../apps/unraid/frontend/platform";
import { openmediavaultBridge } from "../../../../apps/openmediavault/frontend/platform";
import { proxmoxBridge } from "../../../../apps/proxmox/frontend/platform";

/** Only TESTS import every provider. Production builds import exactly one. */
export const ALL_BRIDGES = [
  genericBridge, ugosBridge, synologyBridge, truenasBridge,
  unraidBridge, openmediavaultBridge, proxmoxBridge
];
