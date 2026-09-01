# Contributing

Thank you for your interest in contributing to nest-to-ONVIF.

## Before submitting

Run `make test` before opening a pull request. Tests are required for new behaviour.

`gofmt` is enforced by CI. Run `make fmt` to format your code locally.

## Secrets

Never commit `config.yaml` or `tokens.json`. These files contain credentials and OAuth tokens.
