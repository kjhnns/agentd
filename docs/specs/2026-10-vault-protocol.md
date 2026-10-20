# Vault protocol v1 (vaultd, `/vault/v1/*`), spec draft 3, 2026-10-20

Plan: ~/clawd/wiki/pages/password-vault-plan-2026-10.md (v5, codex rounds 1 to 5).
Server: `vaultd`, a separate binary in its own Go module (`vaultd/`, module
`github.com/kjhnns/agentd/vaultd`, go 1.26 for `crypto/hpke`). It shares no
process, config, token or state with agentd. Client: the Vault tab in agentd-ios
(phase 2 and 3). Public URL: https://agent.gojoe.run/vault/v1/...

Decisions (Joe, 2026-10-20): a Vault tab in the Agent app; no AutoFill;
generated passwords stay (no expiry, no delete); every password Joe gives can be
deleted.

Draft 2 folds in the codex spec review r1 (24 findings, [S1]..[S24]). Draft 3 folds in
r2 (20 findings, [R1]..[R20]); section 19 is normative and wins over older text.
Implementation: `vaultd/` (Go), tests `go test -race ./...` in `vaultd/`.

## 0. Changes against the wiki plan v5

- vaultd is its own process, not routes inside agentd: vault traffic never meets
  conversation handling, the watch store or the Telegram mirror, and phase 4 can
  move it to its own unix user without touching agentd.
- Given items are excluded from every backup (section 12), so a restore can never
  bring a deleted item back. The phone-side delete list and the restore gate of
  plan v5 are not needed.
- agentd-safe holds a one-time re-encrypted snapshot of pass (2026-09-26) with no
  sync job. Given items never reach it. If a sync is ever added, it must skip
  `vault/`.
- Box key rotation is replaced by "new keys + pair again" (section 9.5) [S7, S8].
  Nothing is lost: given items are encrypted at rest by the GPG store, not by a
  vault key, and shared items are sealed fresh per request.
- Writes do not use `pass insert`/`pass rm`; vaultd writes the same file layout
  itself with gpg, crash-safe (section 13) [S3, S4].

## 1. Threat model and guarantees

Protected against:
- The VPS and Caddy (they end TLS): they see signed envelopes and HPKE ciphertext
  only. They cannot forge a box response, replay a request, or read a value.
- Telegram: the pairing message carries a single-use secret and a key fingerprint,
  never a value. An attacker who can READ Telegram cannot pair without being an
  iOS node of Joe's tailnet. An attacker who can REPLACE the Telegram message
  cannot make the phone pair with fake box keys unless they ALSO control the
  phone's network path to the box's tailnet address (section 10.1) [S1].
- Chat transcripts, the memory service and the audit log: no value ever passes
  through them. Metadata rule [S24]: metadata the phone sends (label, login, url)
  never leaves vaultd except to the phone, with one exception: `vaultd list` shows
  labels of given items to the agent on the box, because the agent must pick the
  right item. Metadata the agent supplies (labels, pass paths of shared items) is
  agent data and may appear in transcripts.
- A network attacker on the tailnet: every vault call needs a paired device key.
- A lost phone: Secure Enclave keys behind Face ID (section 15). After a loss Joe
  runs `vaultd unpair`; the old phone keeps what it already decrypted, so Joe
  rotates those passwords at their services.

NOT protected (accepted, plan phase 4):
- Any process running as user johannes on the box (every agent session, with
  passwordless sudo) can read the box keys, the state dir and the pass store.
- An agent session that decrypts `vault/given/<id>` itself and prints it. Agents use
  `vaultd exec` instead (section 14); this is policy, not enforcement.
- Denial of service through the public route (Caddy and the global limiter cap
  it, section 11).

## 2. Components

```
iPhone (Agent app, Vault tab)
  |  HTTPS, Authorization: Bearer <route token>, signed envelope
  v
agent.gojoe.run  (Caddy on the VPS; new block: /vault/v1/* -> 100.120.65.8:8790)
  |  tailnet
  v
vaultd on the box (user johannes, systemd user unit)
  - public listener   100.120.65.8:8790   /vault/v1/* except /vault/v1/pair/*
  - pairing listener  100.120.65.8:8791   /vault/v1/pair/* only (Caddy never forwards it)
  - admin socket      $XDG_RUNTIME_DIR/vaultd/admin.sock (dir 0700, socket 0600, peer uid check)
  - state dir         ~/.local/state/vaultd (0700): state.json, audit.jsonl, keys/
  - store             ~/.password-store (fixed in production; another root only with test_mode)
  - gpg               /usr/bin/gpg (absolute path), PATH for children = /usr/bin:/bin
```

The phone app has two origins BUILT IN (not taken from any message): the API
origin `https://agent.gojoe.run` and the pairing origin
`http://box.taila526cc.ts.net:8791`. It follows no redirect on either.

## 3. Primitives [S20, S21]

- Hash: SHA-256, one application. "H(x)" = SHA-256 of the bytes x.
- Signatures: ECDSA P-256 over H(message bytes). Wire form: exactly 64 bytes, r
  then s, each a 32-byte big-endian unsigned integer, each in [1, n-1]. High-S is
  accepted on both sides (no normalization). Go: split, `ecdsa.Verify(pub, H(m),
  r, s)`. CryptoKit: `P256.Signing.ECDSASignature(rawRepresentation:)` and
  `isValidSignature(_:for:)` over the message bytes (CryptoKit hashes with SHA-256).
- HPKE (RFC 9180), mode base, suite DHKEM(P-256, HKDF-SHA256) 0x0010, HKDF-SHA256
  0x0001, AES-256-GCM 0x0002. Each message: one sender context, one seal, one open.
  Go: `hpke.NewSender(pk, kdf, aead, info)` then exactly one `Sender.Seal(aad, pt)`;
  `hpke.NewRecipient(enc, sk, kdf, aead, info)` then exactly one
  `Recipient.Open(aad, ct)`. Never the package-level `hpke.Seal`/`hpke.Open` (they
  carry no AAD). CryptoKit: `HPKE.Sender(recipientKey:ciphersuite:info:)` and
  `seal(_:authenticating:)`; `HPKE.Recipient(privateKey:ciphersuite:info:encapsulatedKey:)`
  and `open(_:authenticating:)`. Wire form: enc (65 bytes) || AEAD ciphertext ||
  16-byte tag; no separate nonce. Minimum length 81 bytes.
- HKDF-SHA256 (RFC 5869); HMAC-SHA256; MAC compare in constant time.
- Public keys: uncompressed SEC1 points, exactly 65 bytes, first byte 0x04,
  validated as P-256 points.
- b64: base64url without padding; a decoder rejects padding, other alphabets and
  non-canonical trailing bits.
- Ids (item, rid, pid, gen): 16 random bytes, written as 32 lowercase hex ASCII
  characters. Key ids: hex of the first 16 bytes of H(public point): `dev` (phone
  signing key), `pkid` (phone agreement key), `bsid` (box signing key), `bkid`
  (box intake key). Wherever an id enters a crypto input, it enters as its 32
  ASCII characters, except the HKDF salt (section 10.2), which is the 16 decoded
  bytes.
- Integers in crypto inputs (AAD versions): unsigned decimal ASCII, no leading
  zero, "0" for zero.
- Time: Unix milliseconds.

Labels. Every signed, MACed, hashed-for-identity, AAD or info input starts with
one of these exact ASCII strings. No label is a prefix of another.

| Use | Label (exact bytes, `\n` = 0x0A) |
|---|---|
| request signature | `agentd-vault-v1 req\n` |
| response signature | `agentd-vault-v1 resp\n` |
| box key fingerprint | `agentd-vault-v1 box-keys\n` |
| pairing: keys answer signature | `agentd-vault-v1 pair-keys\n` |
| pairing: phone signature over T | `agentd-vault-v1 pair-t\n` |
| pairing: phone hello MAC | `agentd-vault-v1 pair-hello\n` |
| pairing: box answer signature and MAC | `agentd-vault-v1 pair-box\n` |
| pairing: phone confirm signature and MAC | `agentd-vault-v1 pair-confirm\n` |
| pairing: box done signature | `agentd-vault-v1 pair-done\n` |
| pairing key HKDF info | `agentd-vault-v1 pair-key` |
| HPKE info phone to box | `agentd-vault-v1 p2b` |
| HPKE info box to phone | `agentd-vault-v1 b2p` |
| AAD phone to box | `agentd-vault-v1 aad-p2b|` (then fields) |
| AAD box to phone | `agentd-vault-v1 aad-b2p|` (then fields) |

## 4. JSON rules [S22]

The signer serializes an object once and signs those exact bytes; the verifier
keeps the bytes. Nobody re-serializes. Rules for every JSON object in this
protocol (headers, payloads, pairing bodies, envelopes):

- At most 64 KiB (requests) or 1 MiB (responses); nesting depth at most 4.
- UTF-8 only. One top-level object, nothing after it except JSON whitespace.
- No duplicate key at any depth. No unknown key. Every key of the schema present.
- No `null`. Integers only: `0` or `[1-9][0-9]*`, at most 2^53 - 1.
- String escapes: only `\"`, `\\` and `\/`. `\u`, `\b`, `\f`, `\n`, `\r`, `\t` are
  rejected (no field may hold a control character, so no escape is ever needed;
  this also rules out unpaired surrogates). Raw bytes 0x00 to 0x1F inside a string
  are rejected (JSON already forbids them).
- String limits are in UTF-8 bytes. Fields of type id: exactly 32 chars `[0-9a-f]`.
  b64 fields: exact decoded lengths as stated.
- Order of work on the box: size check, strict structural parse (bounded), then the
  signature check, then semantic checks. The parser is the same code for all.

Envelope (every request and response on the public listener):
`{"h": b64(header bytes), "s": b64(64-byte signature)}`.
Pairing bodies have their own schemas (section 10).

## 5. Requests (phone to box)

`POST`, no query string, `Content-Type: application/json`,
`Authorization: Bearer <route token>`.

Request header schema (every key present in every request):

| Key | Type | Rule |
|---|---|---|
| `v` | int | 1 |
| `srv` | str <= 64 | `agent.gojoe.run` (config `server_id`) |
| `op` | str | the op of the route (table) |
| `path` | str | equals the HTTP path |
| `dev` | id | phone signing key id |
| `gen` | id | the device's pairing generation |
| `rid` | id | request id, fresh per logical request, the same on a retry |
| `ts` | int | Unix ms at signing |
| `item` | id or "" | target item, or the list cursor (see list ops) |
| `iv` | int | expected current version (replace, delete), else 0 |
| `opid` | id or "" | op.status: the rid of the operation asked about, else "" |
| `bkid` | id or "" | intake and replace: the box intake key id, else "" |
| `c` | b64 or "" | intake and replace: HPKE ciphertext (81 to 4096 bytes decoded), else "" |

Signature: the phone signing key over `"agentd-vault-v1 req\n" || header bytes`.
Request hash: `rh = H(header bytes)` (32 bytes; b64 on the wire).

Routes, ops and the authorization table [S12]:

| Path | `op` | Required non-empty | Item kind allowed | Effect |
|---|---|---|---|---|
| /vault/v1/intake | intake | item (new, phone-chosen), bkid, c | none yet (creates `given`) | given item, version 1 |
| /vault/v1/replace | replace | item, bkid, c; iv >= 1 | `given`, live | new value, version iv+1 |
| /vault/v1/delete | delete | item; iv >= 1 | `given`, live | tombstone, file removed |
| /vault/v1/given/list | given.list | (item = cursor or "") | `given`, live | page of given items |
| /vault/v1/shared/list | shared.list | (item = cursor or "") | `shared`, live | page of shared items |
| /vault/v1/shared/get | shared.get | item | `shared`, live | the value, sealed |
| /vault/v1/op/status | op.status | opid | n/a | state of the operation `opid` |
| /vault/v1/unpair | unpair | | n/a | this device is revoked |

Fields not listed as required for an op must be "" / 0; anything else is
`bad_request`. Access to given items belongs to the paired device of the moment:
after a new pairing the new device sees and manages every live given item.

Check order on the box (the first failure ends the request):
1. Route known and on the right listener, method POST, no query, body <= 64 KiB,
   else unsigned 404 / 405 / 400 / 413.
2. Global limiter (section 11), else unsigned 429.
3. Bearer token equals the route token (constant time), else unsigned 401 and a
   failure is counted.
4. Envelope and header pass the strict parser; `dev` names an active device; the
   signature verifies with its key. Else unsigned 401, failure counted.
5. Semantic checks: `v`, `srv`, `path`, `op`, `gen` (equals the device's), the
   field rules above. Else signed `err:bad_request`.
6. `ts` in [now - 120 s, now + 30 s], else signed `err:stale`.
7. Device limiter, else signed `err:rate_limited`.
8. The op (section 7 for mutations, section 8 for reads).

An unsigned failure carries no detail (`{"error":"unauthorized"}` etc.). A signed
error is a normal signed response with `status` = `err:<code>` and HTTP 200.
Codes: `bad_request`, `stale`, `rid_conflict`, `rate_limited`, `not_found`,
`version_conflict`, `exists`, `tombstoned`, `wrong_kind`, `decrypt_failed`,
`bad_payload`, `full`, `not_ready`, `internal`.

## 6. Responses (box to phone)

Response header schema:

| Key | Type | Rule |
|---|---|---|
| `v` | int | 1 |
| `srv` | str | server id |
| `dir` | str | `resp` |
| `op` | str | the request op |
| `rh` | b64 (32) | the request hash this answers |
| `dev` | id | device |
| `gen` | id | generation |
| `bsid` | id | box signing key id |
| `ts` | int | Unix ms |
| `status` | str | `ok` or `err:<code>` |
| `opid` | id or "" | mutations and op.status: the operation id (= its rid) |
| `state` | str | mutations and op.status: `applying`, `done`, `failed`; else "" |
| `code` | str | when state is `failed`: the error code; else "" |
| `item` | id or "" | the item |
| `iv` | int | the item version after the op (given), else 0 |
| `entries` | array | list ops: up to 50 entries; else [] |
| `next` | id or "" | list ops: cursor for the next page, "" at the end |
| `c` | b64 or "" | shared.get: the sealed value; else "" |

Entry schema: `{"item": id, "iv": int, "created": int, "updated": int, "last_use":
int, "c": b64}`; `c` is the item metadata sealed to the phone.

Signature: the box signing key over `"agentd-vault-v1 resp\n" || header bytes`.

The phone accepts a response only if the signature verifies with the pinned box
signing key, `v`=1, `dir`=`resp`, `srv`, `op`, `dev`, `gen` and `bsid` match its
own, `rh` equals H of the header it sent, and the op-specific fields are
consistent (for example `opid` = its rid on a mutation). Otherwise it shows "not
verified" and changes nothing. `ts` is for display only (`rh` already binds the
request; a replayed old response for the same `rh` is the same answer).

## 7. Mutations: reservation, item lock, journal [S2, S5, S6, S11]

Operation id = the request's `rid` (chosen by the phone before sending). So the
phone always knows it, even when the response is lost.

State lives in `state.json` (write: temp file in the dir, fsync, rename, fsync
dir). One mutex M guards the in-memory state. A device lock D (RWMutex): every
request holds D for reading from step 4 to its reply; `unpair`, `pair` completion
and `reset-keys` take D for writing, so they wait for in-flight requests and no
request runs across a revocation.

Mutation flow:
1. Under M: look up `rid`.
   - Known, same `dev` and same `rh`: return the op's current answer (state
     `applying`, `done` or `failed`; a done op returns its stored signed response
     bytes unchanged). No work.
   - Known with another `dev` or `rh`: signed `err:rid_conflict`.
   - Unknown: insert an in-memory reservation for `rid` (not yet durable), so a
     concurrent copy of the same request gets "applying".
2. Acquire the per-item lock of `item` (in memory; held until step 5).
3. Under M: validate against the CURRENT item (kind, status, version, the
   authorization table); on failure drop the reservation, reply the signed error.
   Otherwise write the op record durably with state `applying`:
   {rid, dev, gen, rh, header bytes (includes `c`), op, item, iv, created}. For
   intake, also create the item record with status `pending`. For delete, set the
   item status to `tombstoned` in the SAME write (a later replace sees it).
4. Side effect (outside M, item lock held): decrypt `c` with the intake key and
   the AAD, validate the payload, write the file (section 13); or remove the file.
5. Under M: finish. Intake/replace: item live, version, `updated`. Delete: nothing
   more. Op state `done` (or `failed` + code), the header bytes dropped (the
   ciphertext goes with them), the signed response stored, state.json written.
   Release the item lock. Reply.

Failures in step 4:
- decrypt or payload invalid: op `failed` (`decrypt_failed` / `bad_payload`),
  intake's pending item record removed (the id stays usable; it never held data),
  nothing written.
- gpg or file system error on a write: the old file is untouched (temp + rename),
  so the op is `failed` (`internal`) and the item stays as it was.
- removal error: the op stays `applying` (the item is already tombstoned); vaultd
  retries every minute and at start. The phone sees `applying` via op.status.

Recovery at start: every op in `applying` is run from step 4 using the stored
header bytes (dev, gen, bkid, AAD all derive from them). A write is idempotent
(same content again); a removal treats "file absent" as done. Ops of a device that
was revoked meanwhile are still completed (they were authorized when accepted).

op.status: looks up `opid` among the ops of the requesting `dev` (any generation
of the same device key). Unknown: `err:not_found`. Answer: `opid`, `state`,
`code`, `item`, `iv`. A retry outside the time window is not possible (`stale`);
the phone uses op.status with a fresh rid instead.

### Payloads [S21, S23]

Intake and replace plaintext (phone to box), JSON (section 4 rules):
`{"label": str 1..100, "login": str 0..200, "url": str 0..500, "password": str 1..1024}`.
No string may contain a character below U+0020 or U+007F. File content written:

```
<password>\n
login: <login>\n
url: <url>\n
label: <label>\n
```

Shared value (box to phone): `{"label","login","url","password"}`, same limits.
Item metadata in entries (box to phone): `{"label","login","url"}`.

### AAD (ASCII bytes)

- Phone to box: `agentd-vault-v1 aad-p2b|<srv>|<dev>|<gen>|<bkid>|<op>|<item>|<version>`
  where version is the item version the ciphertext will become (1 for intake, iv+1
  for replace).
- Box to phone: `agentd-vault-v1 aad-b2p|<srv>|<dev>|<gen>|<pkid>|<kind>|<item>|<version>`
  with kind `shared.value`, `given.meta`, `shared.meta` or `pair.box`. Shared items
  have version 0.

No field can contain `|`: ids, decimal ints and fixed names only, and `srv` is
validated against `^[a-z0-9.-]{1,64}$`.

## 8. Reads, pagination, retention [S16, S17]

- given.list / shared.list: live items of that kind ordered by item id; the page
  starts after the cursor `item` ("" = start); up to 50 entries; `next` = the last
  id of the page when more follow. The list is not a snapshot: items created or
  deleted between pages may appear or vanish, which the phone handles by
  refreshing. The encoded response must be <= 1 MiB; vaultd shrinks the page if
  needed (metadata limits make 50 entries fit far below that).
- shared.get: the value is read from the pass entry at request time and sealed.
  `last_use` is updated.
- Reads store `rid -> (dev, rh, ts)` only (no response bytes) for 10 minutes. A read
  rid is single use: any second use is `rid_conflict` [R11].

Retention and bounds:
- Live items: at most 500 per kind (`err:full`).
- Op records: done or failed ones are pruned after 7 days; at most 2000 records;
  when full, new mutations get `err:full`.
- Read records: pruned after 10 minutes; at most 5000 (oldest dropped first;
  safe because the timestamp window is shorter).
- Tombstones: item id and time only, kept for ever (a tombstoned id is never
  reused); at most 20000, then intake answers `err:full`.
- Pairing: at most one record at a time (section 10).
- state.json is rewritten whole; with these bounds it stays below a few MiB.

## 9. Keys

### 9.1 Box keys
One box signing key (ECDSA P-256) and one box intake key (HPKE P-256), created on
first start: `keys/sign.key` and `keys/intake.key` (32-byte scalars, 0600, written
temp + fsync + rename + fsync dir BEFORE state.json refers to them).

### 9.2 Fingerprint
`fp = H("agentd-vault-v1 box-keys\n" || bsig_pub || bint_pub)` (65 + 65 bytes).
`vaultd status` prints all 64 hex characters in 16 groups of 4; the app shows the same [R18].

### 9.3 Phone keys
Two Secure Enclave keys: signing (`SecureEnclave.P256.Signing.PrivateKey`) and
agreement (`SecureEnclave.P256.KeyAgreement.PrivateKey`, which conforms to
`HPKEDiffieHellmanPrivateKey`). See section 15 for access control.

### 9.4 Unpair
`unpair` (phone) or `vaultd unpair <dev>` (admin): takes D for writing, marks the
device revoked, writes state, closes any open pairing record. A revoked device id
is never active again.

### 9.5 Reset keys (replaces rotation)
`vaultd reset-keys --yes`: takes D for writing, creates new box keys, revokes every
device, deletes any pairing record, writes state, deletes the old key files. Joe
pairs again. Given and shared items are unaffected.

## 10. Pairing [S1, S9, S10, S18]

At most one pairing record exists. `vaultd pair-start` replaces an older open
record. With `max_devices` = 1 and an active device, `pair-start` needs
`--replace`; the old device is revoked when the new pairing completes, not before.

### 10.1 Start
The box creates the record: pid, S (32 random bytes), expiry now + 5 min,
attempts 0, state `open`, the current bsid and bkid (pinned; a `reset-keys`
deletes the record). It prints:

```
agentd-vault://pair#<b64 of P>
P = {"v":1,"pid":id,"s":b64(S, 32),"fp":b64(fp, 32),"exp":int}
```

P carries no URL, no token and no key: the phone uses its built-in pairing origin
and gets the keys from the box (10.2), then checks them against `fp`. The agent
sends the URI to Joe through the Telegram reply tool; Joe taps it on the iPhone
(the app also accepts a paste).

Why this holds [S1]: a reader of Telegram learns S and fp, but pairing also needs
a request from an iOS node of Joe's tailnet (10.3, check 1). A replacer of the
Telegram message can give the phone another S and fp, but the keys come from the
pairing origin `box.taila526cc.ts.net:8791` over the tailnet; to make the phone
accept fake keys the attacker must also answer for that name on the phone's
network path (Tailscale off on the phone plus a DNS and routing attack). Both
together are out of scope. Upgrade path: enable Tailscale HTTPS certificates and
move the pairing origin to `https://`, which removes the network-path assumption.

### 10.2 Keys
`POST /vault/v1/pair/keys`, body `{"pid": id}`. Box: pairing limiter, record
`open` or `answered` and not expired, else 404. Answer:
`{"bsid":id,"bsig_pub":b64(65),"bkid":id,"bint_pub":b64(65),"ksig":b64(64)}` with
`ksig` = box signature over `"agentd-vault-v1 pair-keys\n" || pid(32 ASCII) ||
bsig_pub || bint_pub`. The phone checks: points valid, key ids match H(points),
fp matches, ksig verifies with bsig_pub. Any mismatch: stop and show "pairing
failed: box keys do not match".

### 10.3 Hello
The phone creates its two keys, pc (16 random bytes) and serializes T once:

```
T = {"v":1,"srv":str,"pid":id,"bsid":id,"bsig_pub":b64,"bkid":id,"bint_pub":b64,
     "psig_pub":b64(65),"pagr_pub":b64(65),"pc":b64(16),"name":str 1..40}
K = HKDF-SHA256(ikm = S, salt = pid as 16 decoded bytes, info = "agentd-vault-v1 pair-key", L = 32)
```

Before sending, the phone durably stores the pending pairing: the key references,
T, K, pid and expiry (Keychain, this device only) [S19].

`POST /vault/v1/pair/hello`, body `{"t": b64(T), "sig": b64(64), "mac": b64(32)}`:
sig = phone signing key over `"agentd-vault-v1 pair-t\n" || T`;
mac = HMAC(K, `"agentd-vault-v1 pair-hello\n" || T`).

Box, in order (a failure answers 403 `{"error":"pairing failed"}`):
1. Pairing limiter (10 calls per minute in total) before any lookup.
2. Record `pid` exists and is not expired. (Unknown pid: 403, no attempt counted.)
3. Tailnet identity of the remote address, `tailscale whois --json` with a 3 s
   timeout: owner login = config `pair_login`, OS = config `pair_os` (`iOS`). The
   node id is bound to the record at the first successful hello; later calls must
   come from the same node.
4. mac verifies with K (else attempts += 1; at 5 the record is deleted).
5. T parses strictly; `pid`, `srv`, `bsid`, `bsig_pub`, `bkid`, `bint_pub` equal the
   record and the current keys; psig_pub and pagr_pub are valid and differ; sig
   verifies with psig_pub. (Else attempts += 1.)
6. Record `answered`: H(T) must equal the bound hash; then the stored answer is sent
   again. Another T: 403, the record is deleted.
7. Record `open`: R = 32 random bytes; rc = HPKE seal to pagr_pub, info
   `agentd-vault-v1 b2p`, AAD `agentd-vault-v1 aad-b2p|<srv>|<dev>|<pid>|<pkid>|pair.box|<pid>|0`
   (dev, pkid from the phone keys; the pid stands in for gen), plaintext
   `{"r": b64(R, 32), "route_token": str 32..128}`. Under M and durably: state
   `answered`, H(T), R, node id, the answer bytes.

Answer: `{"rc": b64, "bsig": b64(64), "bmac": b64(32)}` with
bsig = box signature over `"agentd-vault-v1 pair-box\n" || T || rc` (rc as decoded
bytes) and bmac = HMAC(K, the same bytes).

### 10.4 Confirm
Phone: verifies bsig and bmac, opens rc with the agreement key (this proves the
agreement key; Face ID prompt), gets R and the route token. It stores both in the
pending pairing.

`POST /vault/v1/pair/confirm`, body
`{"th": b64(H(T)), "csig": b64(64), "cmac": b64(32)}`:
csig = phone signing key over `"agentd-vault-v1 pair-confirm\n" || T || R`;
cmac = HMAC(K, the same bytes).

Box, in order: pairing limiter; record exists and not expired (expiry is checked
here too); same tailnet node as bound; record `answered` or `done`; th equals the
bound hash; cmac verifies with K; csig verifies with psig_pub.
- Record `done`: the confirm bytes must equal the stored accepted confirm; then the
  stored done answer is sent again (no second device).
- Record `answered`: take D for writing; recheck `max_devices` (revoke the old device
  when `--replace` was given, else 403); create the device {dev, pkid, both public
  keys, name, gen = 16 random bytes, paired_at}; record state `done` with the
  accepted confirm bytes and the done answer; S and K are no longer needed and are
  dropped from the record (the done retry compares exact bytes instead of a MAC);
  write state durably; then the notice (section 14, `notify_cmd`).

Done answer: `{"dev":id,"gen":id,"dsig":b64(64)}`, dsig = box signature over
`"agentd-vault-v1 pair-done\n" || T || dev || gen` (ids as ASCII).

The phone stores dev, gen, the box keys and the route token as its active pairing
only after dsig verifies, then deletes the pending pairing. After a crash it resumes
from the pending pairing (hello and confirm retries are idempotent while the record
lives). The record is deleted at expiry.

## 11. Limits [S18]

- Behind Caddy every public request has the same source address, so limits are
  global, not per address.
- Global: 300 public requests per minute; 30 unsigned failures (401) per minute, then
  every unauthenticated request gets 429 for one minute.
- Per device: 60 requests per minute; mutations 20 per hour.
- Pairing listener: 10 calls per minute in total, counted before any lookup or whois.
- HTTP servers: read header 10 s, read 30 s, write 30 s, idle 60 s, max header 8 KiB.

## 12. Backups [S13]

- `workflows/workflows/box_memory_backup.py` `_build_package` copies the pass store
  into the recovery package (10 dated copies on the Mac disk, a Drive file with
  revisions). It must skip `vault/given/` BEFORE reading any file content. Test: a
  fixture store with `vault/given/x.gpg` and `vault/given/sub/y.gpg` is packaged
  without them; other entries stay.
- Restic sources do not include `~/.password-store` or `~/.local/state`.
- Gate: vaultd answers intake and replace with `err:not_ready` unless config
  `backup_exclusion` names the deployed clawd commit of that change (T80 in
  clawd `scripts/run-tests` tests it). Deploy sets it
  after the test passes.
- Restore: a restored box has no `vault/given/` (never packaged) and no vault state;
  vaultd starts fresh (new keys, no device); Joe pairs again and gives the
  passwords he still needs. Shared entries come back as normal pass entries.
- The store root is fixed to `~/.password-store` unless `test_mode` is on.

## 13. Store adapter [S3, S4, S15]

vaultd writes the pass file layout itself (pass compatibility: `pass show` keeps
working on every entry).

- Path rule: `^[A-Za-z0-9_][A-Za-z0-9._-]*(/[A-Za-z0-9_][A-Za-z0-9._-]*)*$`, at most
  200 bytes, no `.` or `..` segment. Given items: exactly `vault/given/<item id>`.
  Paths from the admin socket (generate, share): the same rule, and the first
  segment must not be `vault`.
- Every directory on the way from the store root to the entry must be a real
  directory (Lstat, no symlink); the entry must be a regular file or absent.
- Recipients: the nearest `.gpg-id` from the entry's directory up to the store root
  (pass's rule); lines trimmed, empty and `#` lines skipped; at least one.
- Write: create missing directories (0700, fsync parent); run
  `/usr/bin/gpg --batch --quiet --yes --compress-algo=none --no-encrypt-to -e -r <id>...
  -o <dir>/.vaultd-<random>.tmp` with the plaintext on stdin; check exit 0 and a
  non-empty file; fsync it; rename over `<path>.gpg`; fsync the directory. Any
  failure removes the temp file and leaves the old entry untouched.
- Read: Lstat checks, then `/usr/bin/gpg --batch --quiet --decrypt <path>.gpg`, stdout
  into memory. stderr is never logged with content (only "gpg failed, exit N").
- Remove: Lstat checks; absent = done; `os.Remove`; fsync the directory; Lstat again:
  absent = done, else the op stays `applying`.
- Child environment: `PATH=/usr/bin:/bin`, `HOME`, `GNUPGHOME` if set, `LANG=C`;
  nothing else.
- Shared entry import [S23]: line 1 is the password (1..1024 bytes, no control
  character); later lines `login: `, `user: `, `username: ` or `email: ` give login,
  `url: ` gives url; other lines are ignored and never output. An entry that does
  not fit is refused with "entry format not supported" and no content.
- Shared versioning: shared items have no version (0). `vaultd generate` writes
  through the adapter; edits to the entry outside vaultd are read as they are.

## 14. Admin socket, CLI, notices [S14, S24]

`vaultd <command>` talks to the running daemon over the admin socket (peer uid =
daemon uid via SO_PEERCRED, else the connection is closed).

| Command | Output |
|---|---|
| `vaultd serve` | the daemon |
| `vaultd pair-start [--replace]` | the pairing URI (single use, 5 minutes) |
| `vaultd devices` | dev, name, paired_at, status |
| `vaultd unpair <dev>` | ok |
| `vaultd reset-keys --yes` | ok, new fingerprint |
| `vaultd generate --pass-path P --label L [--login U] [--url X] [--length 24] [--no-symbols] [--force]` | `ok item <id>` |
| `vaultd share --pass-path P --label L` | `ok item <id>` |
| `vaultd list` | id, kind, label, created, last_use |
| `vaultd exec <id> -- <cmd> [args...]` | the exit code |
| `vaultd status` | listeners, fingerprint, device count, item counts, gate state |

- generate: 24 characters by default from a fixed alphabet (letters, digits and
  `-_.!@#%+=` unless `--no-symbols`), crypto/rand, rejection sampling. Refuses an
  existing P unless `--force`.
- exec: the value (line 1 of the entry) goes to the child's stdin followed by `\n`.
  The child's stdout and stderr go to /dev/null by default. `--output redacted`
  passes them through with every exact occurrence of the value replaced by
  `[redacted]`, capped at 64 KiB; this is best effort only (a child that encodes or
  splits the value defeats it), and the child is trusted code that holds the
  secret. The child runs in its own process group without a shell; `<cmd>` is an
  absolute path; the whole group is killed after 120 s or when vaultd exec is
  interrupted. Works on given and shared items.
- Notices: config `notify_cmd` (an argv list, for example
  `["/home/johannes/clawd/scripts/tg-send", "--"]`) gets one fixed text appended:
  `vault: a device paired (<name>)` or `vault: device revoked`. No other notices.
- No CLI output or log line contains a value, a login or a url. Labels appear only
  in `vaultd list`.

## 15. Phone rules (phase 2, binding) [S19]

- Both Secure Enclave keys: access control `[.privateKeyUsage, .biometryCurrentSet]`,
  protection `kSecAttrAccessibleWhenUnlockedThisDeviceOnly`. No software-key fallback
  (a device without a Secure Enclave cannot pair).
- A change of Face ID enrollment invalidates the keys; the app then shows "pair
  again" and drops the pairing.
- One LAContext per user action, invalidated after it; reveal and copy each need
  Face ID.
- The pending pairing is persisted before hello (10.3); tested on hardware: Face ID
  cancel, lock during pairing, app kill between hello and confirm, lost done answer.

## 16. Audit log

`~/.local/state/vaultd/audit.jsonl` (0600, append, fsync): ts, op, dev, item, result
code, listener (`public` | `pair` | `admin`). No labels, logins, urls or values.

## 17. Deployment (after phase 1 review, Joe's go)

- systemd user unit `vaultd.service` (UMask 0077, NoNewPrivileges, Restart=on-failure).
- Caddy on the VPS: a `/vault/v1/*` block to 100.120.65.8:8790, with
  `/vault/v1/pair/*` answered 404 by Caddy itself. Request body limit 64 KiB.
- The route token in `pass vaultd/route-token`, resolved at start.
- Before the first intake: the backup change deployed and the gate set (section 12).

## 18. Test plan (phase 1)

- Unit: strict parser (duplicates, unknown and missing keys, ints, escapes,
  depth, size), b64 strictness, signatures (labels, high-S accepted, out-of-range
  r/s), HPKE (AAD, info, wrong key), every check-order step, timestamp window, rid
  replay and conflict, version conflict, kind table, tombstones, path rule,
  payload limits, pagination.
- A Go phone client with software keys: pairing (keys, hello, retry, confirm,
  confirm retry, wrong T, wrong MAC, wrong fp, expiry, attempts, second node,
  replace), intake, list, replace, delete, shared get, op status, unpair,
  reset-keys.
- Concurrency: two copies of one request; two replaces with the same iv; replace
  racing delete; unpair racing a read.
- Crash recovery: stop after the durable `applying` write; restart completes it.
- Canary: a planted password never appears in state.json, audit.jsonl, logs,
  stdout/stderr of the CLI, or the gpg temp dir after the op.
- Store adapter with a temporary GNUPGHOME and store: pass compatibility
  (`pass show` reads what vaultd wrote), symlink refusal, temp cleanup.
- Test vectors `vaultd/testdata/vectors.json` for the Swift side: HPKE open, ECDSA
  verify, fingerprint, HKDF, HMAC, AAD strings, a full signed request and response,
  a full pairing transcript.

## 19. Draft 3 amendments (codex spec r2), normative

- [R1] Lock order everywhere: D -> applyRun -> path lock -> M. Nobody takes D, applyRun
  or a path lock while holding M; nobody takes D while holding a path lock. Phone
  `unpair` does not run under the request read lock: it takes D for writing and
  verifies the request under it. Pairing confirm, admin unpair and reset-keys take D
  for writing. The path-lock table has its own leaf mutex.
- [R2] A write has two failure classes. Before the rename: the old entry is intact;
  the op is `failed` (`internal`). After the rename (directory fsync failed): the
  outcome is uncertain; the op stays `applying` with its header and the item stays
  busy; the retry loop writes the same content again until it is durable. If
  state.json cannot be written after a side effect, vaultd exits (systemd restarts it)
  and recovers from durable state; it never answers from memory.
- [R3] Removal fsyncs the entry's directory (or its nearest existing ancestor) also
  when the entry is already absent, before `done`.
- [R4] Key files are named by key id (`keys/<bsid>.sign`, `keys/<bkid>.intake`).
  Reset: write the new files durably, then commit state.json (new ids, all devices
  revoked, pairing dropped), then delete files that state does not point to. At start
  every key file state does not point to is deleted. Reset is refused while any op is
  `applying`.
- [R5] Recovery runs inside `New`, before any listener starts. Recovery and the retry
  loop share one runner (applyRun); each op runs under its path lock with its state
  rechecked under M. An item with an op in `applying` is busy (`busy` field, persisted):
  replace and delete on it answer `err:busy`, so a recovering write can never land
  after a later delete.
- [R6] Pairing transitions happen under M after re-reading the record: hello
  re-checks pid, expiry, keys and node, and an `answered` or `done` record only
  answers a retry with the same H(T). Confirm verifies, then takes D for writing, then
  under M re-checks record identity, state `answered`, H(T), node, expiry, keys and
  capacity before it commits.
- [R7] The record stores the exact T bytes (and with them both phone public keys), R,
  the node id, the replace flag and the answer, durably, before the answer is sent.
- [R8] Confirm on a `done` record compares the exact request body with the stored
  accepted confirm and returns the stored done answer; this branch runs before any
  MAC check (S is gone). The phone serializes the confirm body once, stores it with
  the pending pairing, and retransmits those exact bytes.
- [R9] Store temp files are named `.vaultd-tmp-<random>`; at start, before serving,
  vaultd removes every such file in the store and every `.tmp-*` file in its state
  and keys dirs. One daemon per state dir (flock on `state_dir/lock`).
- [R10] Request identity: one rid namespace for mutations and reads. A duplicate of
  an in-flight mutation waits (up to 45 s) for the first copy and answers from its op
  record. A mutation that fails validation is recorded as a terminal op
  (`failed`, `validation`), so a retry of that rid gets the same error. `op.status`
  answers `not_found` only for a rid that never reached the journal; the phone then
  sends the mutation again with a NEW rid after checking the item list.
- [R11] Freshness: every read and every status query uses a fresh rid. The phone
  never lets an `applying` answer replace a terminal state it already holds.
- [R12] Path locks are per canonical store path and are taken by mutations,
  shared.get, generate, share and exec. generate checks existence and writes under
  the same lock. Given metadata (label, login, url) is kept in state.json next to the
  version, written in the same commit, so a list is one consistent snapshot. exec is
  authorized when the value is handed to the CLI; a later delete does not reach a
  child that already has it.
- [R13] gpg output is buffered privately, capped at 64 KiB, and used only after exit 0;
  on any failure the buffer is wiped and dropped.
- [R14] Supported pass configuration: `.gpg-id` recipients with inline `#` comments
  stripped (as pass does). vaultd refuses to start when PASSWORD_STORE_KEY,
  PASSWORD_STORE_SIGNING_KEY or PASSWORD_STORE_GPG_OPTS is set, and refuses a write
  under a directory whose `.gpg-id` has a `.gpg-id.sig`. The user's normal gpg.conf
  applies, as for pass.
- [R15] Bounds on work: at most 8 public requests in work (others wait 5 s, then 503);
  gpg runs in its own process group with a 30 s deadline and is killed as a group;
  exec children likewise with 120 s; notices run after the durable commit, outside
  every lock, with a 60 s deadline.
- [R16] Capacity is reserved under M: pending intakes count as live. Byte budget:
  headers live only while `applying`; finished ops keep a signed answer of about 1 KiB;
  2000 ops, 20000 tombstones, 1000 items and 20 revoked devices keep state.json under
  about 5 MiB. Deletes stay possible when the op table is full. The audit log rotates
  at 10 MiB (one old file).
- [R17] Hard links: the adapter refuses an entry with more than one link (given,
  shared and generate). The backup filter skips symlinks and multi-link files.
  External tools must not alias entries.
- [R18] Fingerprint: all 64 hex characters. Intake and replace require `bkid` to equal
  the current intake key id before decryption. `iv` must be at most 2^31. Unlabelled
  hashes (exceptions to the label rule): key ids, `rh` = H(header), `th` = H(T).
  Every failed op answers `status` = `err:<code>` with `opid`, `state` = `failed` and
  `code` set; other errors carry only the common fields.
- [R19] Shared metadata: for login (keys login, user, username, email) and url, the
  first matching line wins. Admin input gets the wire limits (label 1..100, login
  <= 200, url <= 500 bytes, no control character); generate length is 12..128.
- [R20] Phone plaintext (binding for phase 2/3): the Vault tab shows an opaque cover
  on `.inactive`; revealed values are dropped on lock, on leaving the tab and after
  60 s; no plaintext cache or log; copy uses `UIPasteboard` with `.localOnly` and an
  expiration of 60 s; screenshots of a revealed value are not prevented but the
  cover hides the app switcher snapshot. Hardware tests: switcher snapshot, lock
  after reveal, clipboard expiry, no plaintext in the app container.
- Replicas [S13, R]: the inventory is (1) the recovery package (filtered, section 12),
  (2) restic (no store, no vault state), (3) agentd-safe (one-time snapshot of
  2026-09-26, no sync), (4) manual `rsync -a ~/.password-store/` copies described in
  wiki minipc-linux-agent-host (a one-time host move; any future run must add
  `--exclude vault/given/`).

## 20. Amendments from the code review r1 (codex, 30 findings, [C1]..[C30]), normative

- [C1] An op whose write may be in place (ErrUncertain once, or any op found
  `applying` at start) is marked `uncertain` (persisted). An uncertain op never ends
  `failed` because of a later write error; it retries the same content until done.
  Only a first-attempt failure before the rename is terminal.
- [C2] state.json writes: a failure before the rename leaves the old file and the
  caller rolls its in-memory change back; a failure after the rename stops vaultd
  (it restarts from durable state). Every state-changing caller follows this.
- [C3] The daemon lock file stays open for the daemon's whole life.
- [C4, C6, C19] Pairing confirm checks every rejection (revoked id, reused id,
  capacity) before any change and commits devices, revocations and the record as
  one transaction with rollback. Hello and confirm retries on an `answered` or
  `done` record are matched on the exact bound bytes before any MAC check, and an
  identical confirm that lost the race gets the stored done answer.
- [C5] Pairing reads the box keys as one snapshot under M and rechecks at commit
  that the keys did not change. Every public response is built and signed while D
  is held; D is released before the HTTP write (amends section 7: "until its reply"
  means until the reply bytes exist).
- [C7] A finished op stores the version it produced (`result_iv`) and its signed
  answer, also after recovery; op.status reports the op's own outcome.
- [C8, C10] Admission refusals (`not_ready`, `full`, and any refusal while the op
  table is full) are answered without a journal record and with `opid` "". With a full
  table only a valid delete of a live item is admitted. Intake reserves tombstone
  room for every live given item, and applying headers are capped at 1 MiB in total.
- [C9, C11] generate and share reserve the shared item (status `pending`, durable)
  under M with the capacity check, then write, then mark it live. A start resolves
  pending shared items (entry present: live; else dropped). An uncertain generate
  write is retried with the same value; if it stays uncertain the item is kept and the
  agent is told the entry may hold the new value.
- [C12, C13] Start fails when the temp sweeps (store, state, keys) or the retirement
  of old key files cannot complete. Directory creation fsyncs every parent on each
  write.
- [C14] Read rids are memory only, so a read signed before the current start is
  `stale`. Mutations are protected by the durable op table and are exempt.
- [C15] gpg, exec children and notices run in their own process group with a
  WaitDelay of 5 s; after the leader exits the whole group is killed.
- [C16] The retry runner takes D (read), applyRun, the path lock and M per op, not
  per batch.
- [C17] The strict parser rejects every key that is not spelled exactly as in the
  schema (Go's decoder would match case-insensitively).
- [C21] Shared import: line 1 is the password byte for byte (a trailing CR makes the
  entry unsupported); keys `login`, `user`, `username`, `email`, `url` match exactly
  (lowercase); the first line of a field wins even when empty; an invalid first value
  makes the entry unsupported.
- [C22] shared.get returns the value and the login and url of the current entry.
  Lists show the metadata recorded at share or generate time.
- [C24] The pairing listener requires `Content-Type: application/json`, no query, and
  a body of at most 16 KiB (T at most 8 KiB). The public listener requires the same
  content type. A duplicate of an in-flight mutation waits at most 20 s, then answers
  `applying`.
- [C25] The failure limiter counts requests with a wrong token or an invalid
  signature; the 30th in a minute starts a one-minute block in which such requests get
  429. Requests with a valid token are not blocked by it.
- [C26] Every revoked device id is kept for ever (`revoked_ids`); the detailed device
  list keeps the last 20 revoked devices.
- [C27] The character rule also excludes U+2028 and U+2029 (amends section 7).
- [C28] `.gpg-id`: a regular file of at most 4 KiB with 1 to 16 recipients; scanner
  errors and any `.gpg-id.sig` lookup result other than "absent" fail closed.
- [C29] Notices are launched after every protocol lock is released. Audit write,
  sync and rotation failures are logged; a failed rotation truncates the log. The
  listener field of the audit log may also be `recovery`.

## 21. Amendments from the code review r2 ([D2]..[D39]), normative

- [D2, D6] Rollback rule, refined: a change that only makes the box stricter
  (a counted pairing attempt, a deleted or expired pairing record) may stay in memory
  when its save fails before the rename; every other change is rolled back.
- [D31, D32] `vaultd generate` is a journaled op (`generate`): under the path lock
  and M it reserves the item (a new item is `pending`; an existing item keeps its
  metadata), marks it busy and stores the op with the entry sealed to the box intake
  key (HPKE, info `agentd-vault-v1 gen`, AAD `agentd-vault-v1 aad-gen|<item>|<path>`).
  The new label, login and url are published only when the op is done. An uncertain
  write stays `applying`; the retry runner finishes the SAME value. The CLI says so; it
  never advises generating again. reset-keys is refused while it is applying.
- [D33] At start a pending shared item is dropped only when the store says the entry
  is absent; a lookup error or a failed save stops the start.
- [D34] Reads signed at or before start + 30 s (the allowed future skew) are `stale`.
  Cost: reads fail for about 30 s after a restart.
- [D35] A delete journals no header (its recovery needs only the item id). Generate
  blobs count against the 1 MiB budget of applying ops.
- [D36] Start-up sweeps fsync every directory they cover, also when nothing was
  removed in this run.
- [D37] A duplicate of an in-flight mutation is answered `applying` at once; the
  phone asks op.status with a fresh rid.
- [D38] The uncertain mark is persisted when it is set and cleared when the op ends.
- [D25] The 30th unauthenticated failure in a minute is itself answered 429; failed
  signatures count like wrong tokens.
- Backup: path checks followed by a later open are not atomic. Trusted tools do not
  create aliases while the backup runs; a concurrent alias swap is out of scope.
