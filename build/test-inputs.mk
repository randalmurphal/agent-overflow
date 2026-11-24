# What the browser test suites need from the build, declared once:
#
#   - the Makefile includes it: harness-build builds the test binaries of
#     E2E_TEST_PACKAGES, and the targets that run Playwright install their
#     set through scripts/playwright-browsers.sh;
#   - scripts/playwright-browsers.sh installs a set by its name here, for
#     the CI image, .github/actions/setup and those Make targets;
#   - ci/toolchain-inputs.sh names the image by CI_IMAGE_BROWSERS and this
#     file's digest, so a changed set builds a new image;
#   - scripts/ci-suites.mjs reads E2E_TEST_PACKAGES, whose test files are
#     Playwright inputs.
#
# Plain `NAME := words` lines only: each reader parses them itself.

# Packages whose test binaries the Playwright suite runs, built as
# bin/ao-<name>-test.
E2E_TEST_PACKAGES := ./internal/frontendclient ./internal/store

# Browser sets, as `playwright install` names them. chromium is both
# builds: the headless shell pages run in, and the regular Chromium that
# specs launch themselves with --headless=new
# (e2e/tests/browser-lifecycle-helpers.ts).
#
# The Playwright suite (playwright.config.ts): make e2e, make
# e2e-mobile-browser and CI's Playwright shards.
E2E_BROWSERS := chromium webkit
# The release upgrade check and --freeze-repro (playwright.manual.config.ts).
E2E_BROWSERS_MANUAL := chromium-headless-shell
# Vitest's browser project (frontend/vitest.config.ts, headless chromium).
VITEST_BROWSERS := chromium-headless-shell
# The CI image: every set above but the regular Chromium build, which only
# the Playwright shards launch. That build is 403 MiB installed, which every
# Linux job would pull with the image; the shards install it from the CDN.
# Both Chromium builds need the same OS libraries, which the image installs.
CI_IMAGE_BROWSERS := chromium-headless-shell webkit
