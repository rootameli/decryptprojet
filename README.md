# Zessen Go

Outil Go pour envois contrôlés (newsletter, notification, relance, OTP) avec profils rotatifs, reprise, journaux asynchrones et test SMTP.

## Arborescence
- `cmd/zessen-go/main.go` : CLI (`run`, `test-smtps`).
- `internal/config` : chargement/validation config JSON.
- `internal/smtp` : session SMTP persistante et construction des messages.
- `internal/worker` : queue de jobs, circuit breaker, retry/backoff, rotation de profil par batch.
- `internal/state` : checkpoint/reprise.
- `internal/logging` : writer JSONL asynchrone.
- `internal/metrics` : stats + affichage console.
- `templates/` : exemples HTML (profils A/B/C).
- `config.example.json` : configuration type.

## Build
```
# Linux/macOS
GOOS=linux GOARCH=amd64 go build -o zessen-go ./cmd/zessen-go
GOOS=darwin GOARCH=amd64 go build -o zessen-go-darwin ./cmd/zessen-go
# Windows
GOOS=windows GOARCH=amd64 go build -o zessen-go.exe ./cmd/zessen-go
```

## Préparer les données
- `leads.txt` : 1 email/ligne
- `smtps.txt` : `host|port|user|pass|mailfrom` par ligne (100+ supportés)
- `templates/` : `profileA.html`, `profileB.html`, `profileC.html`
- optionnel : `subjects.txt`, `fromnames.txt` si vous souhaitez enrichir les pools dans la config

## Commandes
- Test non intrusif des SMTP :
```
zessen-go test-smtps --config config.example.json
```
- Exécution (dry-run possible) :
```
zessen-go run --config config.example.json --dry-run
```
- Reprise :
```
zessen-go run --config config.example.json --resume
```

Options utiles : `--max-workers`, `--domains-allowlist`, `--limit-per-smtp`, `--duration-limit`.

> Le mode `run` refuse de démarrer sans allowlist de domaines : renseignez `domains_allowlist` dans la config (ou `--domains-allowlist`). Le `--dry-run` peut s'exécuter sans allowlist pour faciliter les tests.

## Journaux et reprise
- `logs/sent.log`, `logs/failed.log`, `logs/smtp.log` (JSONL) et `logs/run_summary.json` sans secrets.
- `state/state.json` contient la progression (`done/pending`, tentatives, état SMTP healthy/cooldown/disabled`). Un snapshot est pris automatiquement toutes les ~3s et à la fin du run (écriture atomique).
- `--resume` recharge le dernier snapshot, conserve le `batch_id` et ne réenfile que les leads restants (sans doublon). Les tentatives déjà effectuées sont conservées.
- `--duration-limit` coupe le run via un context timeout et force un checkpoint final pour reprise ultérieure.
- `--limit-per-smtp` désactive un worker après N envois pour ce SMTP (pratique en pré-production/tests).

## Tests
```
go test ./...
go test -race ./...
```
