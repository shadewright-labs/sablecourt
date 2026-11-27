# Licence files carried by the harness images

These files are copied into the images by the embedded Dockerfile.

- `codex/LICENSE` and `codex/NOTICE`: fetched from the openai/codex repository at tag `rust-v0.172.0`, the tag of the Codex CLI release the codex image installs. Update them with the version in `images.go`.
- `go/LICENSE`: fetched from the golang/go repository at tag `go1.27.4`, the toolchain that compiles the council proxy.
- `sablecourt/LICENSE`: a copy of the repository `LICENSE`; a test keeps the two equal.
- `image/<role>.txt`: the `/licenses/README` of each image, listing what the image contains.
