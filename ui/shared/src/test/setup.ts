// jest-dom registers the DOM matchers the tests already use
// (toBeDisabled, toBeEnabled). Without it those assertions do not exist
// and tsc rejects them.
import "@testing-library/jest-dom/vitest";
import "@testing-library/react";
