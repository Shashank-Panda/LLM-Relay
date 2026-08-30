// Not a real module, and nothing here is ever built.
//
// Its only job is to stop `go build ./...`, `go vet ./...` and `go test ./...`
// at the repository root from walking into web/node_modules. npm packages
// occasionally ship Go source of their own — `flatted` carries a complete Go
// package — and without this file that third-party code is compiled as part of
// this project, so an unrelated npm install can break the gateway's build. The
// go tool skips any directory that declares its own module, which is the one
// mechanism that does this; there is no .gitignore or build tag equivalent.
//
// The console is a Next.js app. The only thing Go does under this directory is
// write web/fixtures/dryrun.golden.json, which is plain file I/O and is not
// affected by module boundaries.
module relay-console

go 1.26.4
