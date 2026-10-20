# vaultd

Password exchange between the Vault tab of the Agent iOS app and the pass store
on the box. Own Go module (go 1.26, `crypto/hpke`); shares nothing with agentd.

- Spec: ../docs/specs/2026-10-vault-protocol.md (section 19 is normative).
- Plan and decisions: ~/clawd/wiki/pages/password-vault-plan-2026-10.md.
- Test: `go test -race ./...` (needs `pass` and `/usr/bin/gpg`; uses a throwaway key).
- Interop vectors for the Swift client: testdata/vectors.json
  (`VAULTD_WRITE_VECTORS=1 go test ./internal/phone` regenerates them).
- Deploy files: deploy/ (not installed; spec section 17).

Agent use on the box:

    vaultd generate --pass-path site/password --label "Site" --login joe   # prints: ok item <id>
    vaultd share --pass-path ebay/password --label "eBay"
    vaultd list
    vaultd exec <id> -- /abs/path/to/consumer      # value on the child's stdin
    vaultd pair-start [--replace]                  # URI for Joe, single use, 5 min

Never `pass generate` (it prints the value) and never `pass show vault/given/...`.
