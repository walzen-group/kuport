# kuport — thin generate/verify targets. Every tool runs through the flake's
# devShell (`nix develop -c`); there is no go or controller-gen on the host PATH.

NIX := nix develop -c
CONTROLLER_GEN := $(NIX) controller-gen

.PHONY: generate manifests verify

## generate: regenerate deepcopy functions into the API package.
generate:
	$(CONTROLLER_GEN) object paths=./internal/api/...

## manifests: regenerate CRD manifests into config/crd.
manifests:
	$(CONTROLLER_GEN) crd paths=./internal/api/... output:crd:dir=config/crd

## verify: regenerate CRDs into .tmp/ and fail on any drift from config/crd.
verify:
	rm -rf .tmp/crd
	mkdir -p .tmp/crd
	$(CONTROLLER_GEN) crd paths=./internal/api/... output:crd:dir=.tmp/crd
	diff -ru config/crd .tmp/crd
