# Targets for the loope worker module. Run from worker/.

GO_ARCH_LINT := github.com/fe3dback/go-arch-lint@v1.17.0

.PHONY: test
test:
	go test ./...

.PHONY: vet
vet:
	go vet ./...

# lint-arch enforces the package dependency rules in .go-arch-lint.yml
# (engine must never import infra, shared imports nothing internal, ...).
.PHONY: lint-arch
lint-arch:
	go run $(GO_ARCH_LINT) check

.PHONY: check
check: vet test lint-arch
