# Contributing

Thanks for your interest! This is a small, experimental hobby project, so please keep expectations relaxed: reviews happen on a best-effort basis.

## Ways to help

- **Test on other hardware or firmware.** The project has been tested only on a reMarkable Paper Pro (firmware 6.0.106). Reports from the reMarkable 2 or newer firmware are very welcome, whether it works or not.
- **Report bugs.** Open an issue with your device, firmware version, tunnel mode and the relevant logs:
  ```bash
  journalctl -u rmshare-stream -u rmshare-proxy -u rmshare-tunnel -n 200 --no-pager
  ```
  Remove URLs, auth keys and tokens before posting logs.
- **Security issues** go through [SECURITY.md](SECURITY.md), not public issues.

## Development setup

You need Go 1.26+ (goMarkableStream v1.4.0 requires it; the proxy alone needs 1.22), `curl`, `patch` and `make`.

```bash
make test     # go vet, unit tests, shell syntax check
make build    # cross-compile the proxy (arm, arm64) and pentool (arm64) into dist/
make gms      # download goMarkableStream, apply the patches, build for arm64
```

The proxy's unit tests run on any OS. Features that touch the device (journal, evdev, front light) are tested against fakes: temporary files instead of `/dev/input/event2` and the sysfs backlight.

To try changes on a tablet, run `./deploy.sh` (with a configured `tunnel.env`). For quick iterations on the proxy only:

```bash
make build
scp dist/rmshare-arm64 root@10.11.99.1:/home/root/rmshare/rmshare.new
ssh root@10.11.99.1 'cd /home/root/rmshare && systemctl stop rmshare-proxy && mv rmshare.new rmshare && systemctl start rmshare-proxy'
```

## Guidelines

- Keep the code simple and dependency-free where reasonable. The proxy runs on a small device next to the UI process.
- Format Go code with `gofmt` (`make fmt`). CI checks formatting, vet and tests, and cross-compiles for both ARM architectures.
- Shell scripts run under the tablet's minimal `/bin/sh`: stick to POSIX `sh`, no bashisms.
- Write comments, log messages and UI strings in English.
- Update the README / [docs](docs/) when you change behaviour, flags or configuration.
- Never commit `tunnel.env`, `cloudflared/` credentials or anything in `dist/`.

## Updating the goMarkableStream patches

The patches in `gms/` are plain unified diffs against the upstream release named by `GMS_VERSION`, applied in the order given in `gms/series`. Later patches build on earlier ones, especially the `client_*` patches.

To change a patch or port the series to a new upstream version:

```bash
# 1. Get a pristine upstream tree and put it under git
mkdir /tmp/gms && cd /tmp/gms
curl -fsSL https://github.com/owulveryck/goMarkableStream/archive/refs/tags/v1.4.0.tar.gz | tar xz --strip-components=1
git init -q && git add -A && git commit -qm upstream

# 2. Apply the series one patch at a time, committing after each
for p in $(grep -v '^#' /path/to/repo/gms/series); do
  patch -p1 < /path/to/repo/gms/$p && git add -A && git commit -qm "$p"
done

# 3. Edit (e.g. with git rebase -i), then regenerate each patch from its commit
git diff <parent-commit> <patch-commit> > /path/to/repo/gms/<name>.patch
```

Then run `make gms` (delete `dist/gms-src-*` first: the build reuses an already patched tree), and run the upstream tests with the patches applied: `cd dist/gms-src-v1.4.0 && go test ./internal/stream/`.
