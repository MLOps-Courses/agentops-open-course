# Repository tools

This standalone Go module owns the course's authoring, release, browser, load-fixture, and platform-drill utilities. It deliberately stays separate from the root Hugo module and the production agent module.

Run its local contract with:

```bash
mise run install
mise run format
mise run check
mise run test
```

Root `mise` tasks call the individual commands; contributors normally use the repository-root task vocabulary rather than invoking a command directly.

`github.com/chromedp/cdproto` is held at `dc233986426f` by `chromedp v0.16.0`; accept a newer revision only after the real-Chrome accessibility acceptance passes.
