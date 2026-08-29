import bootstrap from "@platform-entry";

// Every provider shell default-exports a bootstrap function. The shared entry
// knows nothing about which one it is.
bootstrap(document.getElementById("root")!);
