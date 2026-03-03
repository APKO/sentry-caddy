# Variables
Q = $(if $(filter 1,$(V)),,@)
M = \033[34;1m▶\033[0m
GOPATH = $(shell go env GOPATH)

.PHONY: git reset clean reset-github proto

# Clear Git repository (tags, branches, and prune remotes)
git:
	@echo "$(M) Clearing Git..."
	$(Q) git fetch origin --prune
	$(Q) git tag -d $$(git tag -l) 2>/dev/null || true
	$(Q) git branch | grep -v "main" | xargs git branch -D

# Reset Earthly build system
reset:
	@echo "$(M) Resetting Earthly..."
	$(Q) earthly prune --reset || { echo "Earthly reset failed"; exit 1; }

# Clean Go module checksum files
clean:
	@echo "$(M) Cleaning go.sum and go.work.sum..."
	$(Q) find . -name "go.sum" -type f -delete
	$(Q) [ -f go.work.sum ] && rm go.work.sum || true