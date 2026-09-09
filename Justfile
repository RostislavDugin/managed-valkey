run:
    scripts/run_dev.sh run

down:
    scripts/run_dev.sh down

test:
    scripts/run_all_tests.sh

test-e2e:
    just --justfile tests/e2e/Justfile test

test-e2e-headed:
    just --justfile tests/e2e/Justfile test-headed

test-prod:
    just --justfile tests/e2e/Justfile test-prod
