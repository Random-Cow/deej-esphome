# Contributing

Thanks for your interest in deej-esphome! Bug reports, ideas and pull requests are welcome.

## Development setup

You need Go (see `go.mod` for the version) on Linux with PipeWire (`pipewire-pulse`) or PulseAudio. No C toolchain or GTK headers are required.

```sh
make dev            # debug build: verbose, colourful logs on stderr
./deej -v
make all            # gofmt check, go vet, golangci-lint (if installed), tests, release build
```

To try changes without hardware, point `esphome_address` at anything that serves an ESPHome-style `/events` stream. The tests in `pkg/deej/esphome_stream_test.go` show what that stream looks like.

If you change `esphome/deej-esphome.yaml`, validate it with `esphome config esphome/deej-esphome.yaml`. You need a `secrets.yaml` with `wifi_ssid` and `wifi_password` next to it; CI runs the same check.

## Pull requests

- Keep PRs focused; one fix or feature per PR.
- Add or update tests for behaviour changes where practical.
- `make all` should pass. CI runs the same checks.
- Commit messages: short imperative summary line (e.g. `Fix reconnect after WiFi drop`). Prefixes like `fix:` / `feat:` / `docs:` are welcome but optional.

## Releases

Pushing a tag `vX.Y.Z` builds static linux/amd64 and linux/arm64 binaries with GoReleaser and publishes them as a GitHub release.
