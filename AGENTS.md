# Sentinel Agent Workflow

## Branching
- Develop every feature or fix on a dedicated working branch (e.g. `feat/<name>` or `fix/<name>`), not directly on `main`.

## Pre-Merge Checks
Every time you are asked to merge to `main` (or run `cleanup`), all of the following must pass cleanly before merging:
```bash
gofmt -l .          # must output nothing
go vet ./...
go test -count=1 ./...
go build -o bin/webchat .
staticcheck ./...   # must report nothing
gosec -quiet ./...  # must report nothing unannotated; justified exceptions carry // #nosec comments
```

## Cleanup Command
When the user says **cleanup**:
1. Run all pre-merge checks above and ensure they pass.
2. Merge the working branch into `main`.
3. Delete the working branch.
4. Do **not** push to `origin` unless explicitly asked.

## Versioning & Push Commands
Version and changelog entries live in `version.go`:
- **push minor** (or similar): increment the last digit (e.g. `v0.1.0` → `v0.1.1`), add a corresponding `changelog` entry in `version.go`, run all pre-merge checks, merge to `main`, delete the working branch, and push to `origin`.
- **push major** (or similar): increment the second digit and reset the last digit to `0` (e.g. `v0.1.1` → `v0.2.0`), add a corresponding `changelog` entry in `version.go`, run all pre-merge checks, merge to `main`, delete the working branch, and push to `origin`.
