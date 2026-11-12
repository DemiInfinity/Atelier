//! The git client engine on throwaway repositories: staging, history,
//! branches, bringing work together, conflicts, putting aside, worktrees
//! and undo. Every repository lives in a temp folder with its own identity;
//! the real home folder, the real git config and its hooks never apply.

mod kit;

mod branches;
mod conflicts;
mod graph;
mod integrate;
mod stage;
mod stashes;
mod undo;
mod worktrees;
