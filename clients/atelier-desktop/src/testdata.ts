// Shared by the tests: a working copy as the overview sends it.

import type { WorkingCopy } from "./api";

export const copy = (name: string, over: Partial<WorkingCopy> = {}): WorkingCopy => ({
  path: `/code/${name}`,
  name,
  owner: "DemiInfinity",
  owner_name: "DemiInfinity",
  owner_kind: "user",
  pinned: false,
  full_name: `DemiInfinity/${name}`,
  branch: "main",
  changes: 0,
  ahead: 0,
  behind: 0,
  state: "Clean",
  tone: "quiet",
  action: "open",
  short: "",
  worktree_of: null,
  ...over,
});
