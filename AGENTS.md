# AGENTS.md

Repository guidance for coding agents lives in **[CLAUDE.md](CLAUDE.md)** — project
overview, the five-module `go.work` layout, commands (`make test`, `make lint`,
`make check-modules`), architecture, and conventions.

This file is deliberately a pointer rather than a copy. The two were byte-identical
duplicates and had already drifted out of date together: both still documented a
root-only `go test ./...` after the tree became a five-module workspace, so an agent
following either one would "verify" a change without compiling four of the five
modules. One source of truth cannot drift from itself.
