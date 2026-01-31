# Contributing

Thanks for your interest in contributing to **boss-server**.

## License and attribution

By contributing, you agree that your contributions are licensed under the terms of the
`LICENSE` file in this repository. Attribution to **Jonathan Oliver** must be preserved in
any redistribution.

## How to contribute

1) Fork the repo (public forks are allowed with attribution).
2) Create a feature branch.
3) Keep changes focused and well described.
4) Open a pull request with a concise summary and testing notes.

## Development notes

- Runtime services are Go-based.
- Encoding is driven by `/generate_abr/create_abr_ladder.sh` with helper Python scripts.
- UI lives under `content/`.

## Code style

- Go: standard `gofmt`.
- Shell: keep scripts POSIX-friendly where possible.
- HTML/JS/CSS: keep UI changes readable and explicit.

## Testing

Use the UI at `http://localhost:20081/` and verify playback for HLS and DASH.
