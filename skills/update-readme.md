---
name: update-readme
description: >
  Use only when README.md needs a documentation update. Defines the README's
  user-facing structure and how to merge changes without duplicating sections
  or turning prerequisites into a design log.
---

# Updating README.md

Read this file only when a change requires editing `README.md`. It is not
architecture context and is not needed for code, test, or other skill changes.

## What README.md Is For

`README.md` is user-facing. A reader must be able to install the tool,
configure a run, and understand what the run will do. It is not a changelog,
design log, or review record.

Document what the user must provide and what the tool will do. Do not document
why an implementation changed.

## Structure

Keep the existing section order. Add content to the section that answers the
reader's question; do not append a second list.

1. **Intro** — what the tool does in five steps, and the
   no-repository-mutation boundary.
2. **Contents** — links to every section and subsection. Keep it complete when
   adding, renaming, or removing a heading.
3. **Prerequisites** — local tools, accounts and access (including links to
   both fixture repositories), cluster requirements, network access.
4. **Setup** — ordered steps: build, copy and edit the clusters file, verify
   fixture access.
5. **Authentication methods** — browser authentication and configured tokens.
   Provider credential behavior belongs here, not under Running LKMABT.
6. **Running LKMABT** — commands, the cluster GitHub App flow, and run
   behavior.
7. **Run logs and artifact retention** — a commented directory tree for
   `.lkmabt-runs/`, followed by the behaviors a tree cannot show.

Rules:

- Prerequisites are requirements, not behavior. A new behavior belongs under
  Running LKMABT; a new flag belongs in the command or setup section that uses
  it.
- One idea per bullet. Use a table for tool, flag, or variable mappings.
- Prefer a directory tree with inline comments over a bullet list for artifact
  layouts.
- Fence commands as `bash`, configuration as `yaml`, directory trees and exact
  output as `text`.
- Preserve heading text where possible; the Contents list and links point at
  it.
- Do not renumber prerequisites or reorder sections to insert a note.

## Parallel Edits

More than one session may edit `README.md`. Before editing:

1. `git fetch origin` and start from, or rebase onto, the intended base.
2. Re-read the whole README, not only the section being changed.
3. Merge new facts into the existing section; never add a second
   "Prerequisites", "Commands", or "What to expect" block.
4. Resolve a conflict by keeping the structure above and reconciling the
   facts, not by concatenating both sides.
5. Keep a README edit in its own commit when the branch also carries code
   changes.

## Accuracy

- Check every command, flag, environment variable, default, path, prompt, and
  error string against the code on the branch that will merge.
- Document behavior present on `origin/main`. Do not describe unmerged branch
  behavior in a README based on `main`.
- Compare names and defaults with `--help` and the example files
  (`.lkmabt.env.example`, `.lkmabt-clusters.yaml.example`).
- Do not promise access checks, prompts, or cleanup the code does not perform.
- Remove a duplicated fact from its other section instead of keeping two
  copies.

## Before Committing

- Structure matches the list above; no section is duplicated.
- Every command and flag is runnable and spelled as in the CLI.
- Every environment variable and default matches the code and example files.
- No implementation history, rationale, or review references.
- Links and anchors resolve; the document has one H1.
- Fenced code blocks are balanced and language-tagged.
