# Security

This repository publishes no dedicated disclosure contact and no
supported-version matrix.

`toktop update` always fetches the latest GitHub release of the named repo
(default `maci0/toktop`; see `internal/selfupdate`). `--repo owner/name`
redirects that fetch at another public repository, and the downloaded asset
must match the checksums file published in that same release: there is no
signature independent of the release pipeline, so a repository that can
publish a release can ship a binary that verifies. The release workflow
publishes a SLSA provenance entry per file into a transparency log
(.github/workflows/release.yml, the `attest` job), which gives a downloader
an anchor outside that pipeline; `internal/selfupdate` does not read it, so
the automatic updater verifies against the release's own checksums alone.
Older tags are not a support channel.

The attack surface, residual risks, and existing controls are recorded in
[docs/THREAT_MODEL.md](docs/THREAT_MODEL.md).
