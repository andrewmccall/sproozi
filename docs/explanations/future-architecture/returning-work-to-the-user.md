# Returning a session's work to your repository

Status: deferred

## Problem

`sproozi run` copies a repository into a managed workspace, and the agent
works on that copy. Your own directory is never changed. There is no command
that shows what the agent changed or brings those changes back, so today you
have to ask the agent to push them somewhere.

## Desired behaviour

You can see a stopped session's changes against the revision it started from,
then bring them into your repository on a new branch, without Sproozi
touching your working tree or current branch.

## Proposed parts

- A diff of the managed workspace against the session's starting revision,
  covering commits, staged changes and untracked files.
- An export that fetches the session's commits into your repository as a new
  branch, with uncommitted work as a final commit on that branch.
- A record of which session and execution produced each exported branch.

## Open questions

Your repository may have moved on since the session started: new commits, a
rebased branch, uncommitted edits. Should export only create a branch from the
session's base and leave merging to Git, or offer to rebase? What happens when
the starting revision no longer exists locally? Should export refuse while you
have uncommitted changes in the same paths? How does this relate to
[shared workspace reconciliation](shared-workspace-reconciliation.md), where
several agents propose changes to one Project? Do users who run Sproozi without
an agent attached, for example to inspect a workspace, need the same command?

## Scope

This proposal excludes writing into your working tree, automatic conflict
resolution and pushing to a remote.
