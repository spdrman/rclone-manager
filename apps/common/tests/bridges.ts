import { genericBridge } from "../../generic/frontend/platform";
import { ugosBridge } from "../../ugos/frontend/platform";
import { synologyBridge } from "../../synology/frontend/platform";
import { truenasBridge } from "../../truenas/frontend/platform";
import { unraidBridge } from "../../unraid/frontend/platform";
import { openmediavaultBridge } from "../../openmediavault/frontend/platform";
import { proxmoxBridge } from "../../proxmox/frontend/platform";

/** This is the one place in the repo that imports every provider (§63A —
 *  the provider-conformance matrix). It lives here, under apps/common/,
 *  specifically so it is NOT part of ui/shared's own build or test run:
 *  deleting any single apps/<provider>/ must never break ui/shared (EPIC-B
 *  §7.1, docs/EPIC-B-multi-nas.md WP1.1 acceptance criteria). */
export const ALL_BRIDGES = [
  genericBridge, ugosBridge, synologyBridge, truenasBridge,
  unraidBridge, openmediavaultBridge, proxmoxBridge
];
