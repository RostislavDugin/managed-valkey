run:
    scripts/run_dev.sh run

down:
    scripts/run_dev.sh down

format:
    just --justfile api/Justfile format
    just --justfile operator/Justfile format
    just --justfile web/Justfile format

lint:
    scripts/run_all_tests.sh lint

test:
    scripts/run_all_tests.sh test

test-full:
    scripts/run_all_tests.sh test-full

test-integrations:
    just --justfile tests/integrations/Justfile test

test-e2e:
    just --justfile tests/e2e/Justfile test

test-e2e-group group:
    just --justfile tests/e2e/Justfile test-group "{{group}}"

test-e2e-headed:
    just --justfile tests/e2e/Justfile test-headed

test-e2e-prod:
    just --justfile tests/e2e/Justfile test-e2e-prod
