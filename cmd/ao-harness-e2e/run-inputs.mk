# What one run of the Playwright harness suite (e2e/) reads besides the
# suite itself. This file is the declaration every consumer uses:
#
#   - the Makefile includes it: harness-build builds these binaries and
#     records the bundle they embed, `make e2e` installs these browsers;
#   - ci/toolchain-inputs.sh passes E2E_BROWSERS to the CI image, which
#     installs them;
#   - CI packs the harness build's bin/ and frontend/dist for the shards;
#   - bin/ao-harness-e2e embeds it and refuses to start a run, within
#     seconds, while any of it is missing (preflight.go);
#   - scripts/ci-suites.mjs reads E2E_TEST_PACKAGES, whose test files are
#     Playwright inputs.
#
# Plain `NAME := words` lines only: each reader parses them itself.

# Commands harness-build builds into bin/.
E2E_COMMANDS := agent-overflow agent-overflow-noremote ao-harness ao-harness-e2e ao-mockprovider ao-mockforge

# Packages whose test binaries the suite runs, built as bin/ao-<name>-test.
E2E_TEST_PACKAGES := ./internal/frontendclient ./internal/store

# Playwright browsers each kind of run launches, as `playwright install`
# names them. chromium is both builds: the headless shell pages run in, and
# the regular Chromium specs launch themselves with --headless=new
# (e2e/tests/browser-lifecycle-helpers.ts). The CI image installs
# E2E_BROWSERS, which covers the others but Android.
# playwright.config.ts:
E2E_BROWSERS := chromium webkit
# playwright.manual.config.ts and --freeze-repro:
E2E_BROWSERS_MANUAL := chromium-headless-shell
# --flow, headless or --headed:
E2E_BROWSERS_FLOW := chromium
# playwright.android.config.ts:
E2E_BROWSERS_ANDROID := android
