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
