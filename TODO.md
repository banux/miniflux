# Branche `ollama` — Enrichissement et filtrage des articles via Ollama

Objectif : ajouter à chaque article un **score** (basé sur les appréciations passées de l'utilisateur) et des **tags** (extraits du contenu) via un agent Ollama local. Filtrer automatiquement les nouveaux articles dont le score est trop bas, une fois la base de recommandation suffisamment fournie.

## Décisions de design

- Ollama **local** (`http://localhost:11434` par défaut).
- Modèle = celui configuré côté Ollama (paramètre `OLLAMA_MODEL`, l'utilisateur règle).
- Réutiliser le contenu déjà scrapé par miniflux ; ne re-fetcher la page que si le contenu est **trop court** (seuil ~ 500 caractères).
- Enrichissement **asynchrone** (goroutine après `RefreshFeedEntries`) — le refresh reste rapide.
- Filtrage et profil **par utilisateur**.
- Concurrence Ollama bornée (sémaphore global, défaut 1) pour ne pas saturer le serveur local.

## Phase 1 — Fondations ✅

### Schéma & config
- [x] Migration : ajouter `ollama_score real` (nullable), `ollama_tags text[]`, `ollama_enriched_at timestamptz` à `entries`.
- [x] Config dans `internal/config/options.go` :
  - [x] `OLLAMA_ENABLED` (bool master switch)
  - [x] `OLLAMA_URL` (défaut `http://localhost:11434`)
  - [x] `OLLAMA_MODEL` (défaut vide → désactivé)
  - [x] `OLLAMA_TIMEOUT` (secondType, défaut 60s)
  - [x] `OLLAMA_MAX_CONCURRENCY` (int, défaut 1)
  - [x] `OLLAMA_MIN_CONTENT_LENGTH` (int, défaut 500)
  - [x] `OLLAMA_FILTER_THRESHOLD` (int 0–100, défaut 0 = pas de filtre)
  - [x] `OLLAMA_MIN_TRAINING_SAMPLES` (int, défaut 50)
- [x] Accesseurs correspondants.
- [ ] Documenter les variables d'env dans le man `miniflux.1`.

### Modèle & stockage
- [x] `internal/model/entry.go` : ajouter `OllamaScore *float64`, `OllamaTags []string`, `OllamaEnrichedAt *time.Time`.
- [x] `entry_query_builder.go` : SELECT et Scan étendus.
- [x] Nouveau fichier `internal/storage/ollama.go` :
  - [x] `UpdateEntryOllamaEnrichment(entryID, score, tags)`
  - [x] `DeleteEntryAsFiltered(entryID)` (DELETE + tombstone, anti-réingestion)
  - [x] `CountUserRatedEntries(userID)`
  - [x] `GetOllamaUserProfile(userID, limit)`
  - [x] `GetEntryForOllama(entryID)`

### Package Ollama
- [x] `internal/integration/ollama/client.go` : client `/api/chat` non-streaming, format=json.
- [x] `internal/integration/ollama/tags.go` : `ExtractTags`, normalisation des tags, fallback parse.
- [x] `internal/integration/ollama/score.go` : `ScoreEntry`, prompt avec profil, clamp [0,1].

### Pipeline d'enrichissement
- [x] `internal/integration/ollama/worker.go` :
  - [x] Sémaphore global dimensionné via `OLLAMA_MAX_CONCURRENCY`.
  - [x] `EnrichEntries(store, feed, entries)` à lancer en goroutine.
  - [x] Re-scrape si contenu (texte stripped) < `OLLAMA_MIN_CONTENT_LENGTH`.
  - [x] Persistance via `UpdateEntryOllamaEnrichment`.
  - [x] Filtrage : delete + tombstone si `score < threshold` ET assez d'échantillons.
- [x] Branchement dans `internal/reader/handler/handler.go` à côté de `integration.PushEntries`.

### Cache du profil utilisateur
- [x] Cache `sync.Map` avec TTL 5 min, clé = userID (invalidation laissée au TTL pour la v1).

### Tests
- [x] Tests unitaires sur `normalizeTags`, `extractJSON`, `truncate`, `clamp01`.
- [x] Tests d'intégration légers contre un `httptest.Server` simulant Ollama (tags, score, profil vide).

## Phase 2 — Robustesse / observabilité
- [ ] Logs structurés : durée d'inférence, taux d'erreur Ollama.
- [ ] Backoff/retry sur erreur Ollama (1 retry, sinon on laisse l'entrée non enrichie, pas bloquante).
- [ ] Tests unitaires : parsing des réponses Ollama, prompt builders, sélection contenu vs re-fetch.
- [ ] Test d'intégration léger avec un faux serveur HTTP simulant Ollama.

## Phase 3 — UI (futur, hors scope v1)
- [ ] Afficher le score dans la liste d'articles (badge).
- [ ] Toggle par feed dans les settings.
- [ ] Page d'admin pour voir le nombre d'articles filtrés.

## Risques / points de vigilance
- Latence Ollama : un modèle 7B+ peut prendre 5–30 s par article. Avec 100 articles d'un refresh, ça empile. Le sémaphore + l'asynchronisme couvrent, mais il faudra surveiller.
- Le profil utilisateur peut grossir : limiter à ~30 articles récents pour tenir dans le contexte.
- Hallucinations sur les tags : forcer un JSON strict, fallback `[]` si parsing échoue.
- Ne **jamais** bloquer le pipeline RSS si Ollama est down — failure mode propre (logger et continuer).
- Re-fetch : utiliser le scraper existant + `BlockPrivateNetworks` (sécurité SSRF déjà gérée).

## Phase 1bis — Page de revue des articles filtrés ✅

**Décision** : remplacer le delete+tombstone par un soft-filter (colonne `ollama_filtered_at`). Les articles filtrés restent en base et sont consultables sur une page dédiée pour ajuster le seuil et restaurer les faux positifs.

### Schéma & storage
- [x] Migration : ajouter `ollama_filtered_at timestamptz` + index partiel.
- [x] `MarkEntryAsFiltered(entryID)` — pose la marque + force `status='read'`.
- [x] `RestoreFilteredEntry(userID, entryID)` — efface la marque + repasse en `unread`.
- [x] `CountOllamaFilteredEntries(userID)`.
- [x] `EntryQueryBuilder` : par défaut, exclut les articles filtrés. `OnlyOllamaFiltered()` et `IncludeOllamaFiltered()` pour les cas spécifiques.
- [x] Worker mis à jour pour appeler `MarkEntryAsFiltered`.

### UI
- [x] Route `GET /ollama/filtered` (contrôleur + template `ollama_filtered_entries.html`).
- [x] Route `POST /ollama/filtered/{entryID}/restore`.
- [x] Lien dans le menu principal (visible seulement si `ollamaEnabled`).
- [x] Helper template `ollamaEnabled`, `deRefFloat`, `deRefTime`.
- [x] Clés i18n ajoutées (en/fr complètes, fallback anglais sur les 20 autres langues avec bon nombre de pluriels).

## État courant
- [x] Branche `ollama` créée.
- [x] TODO.md rédigé.
- [x] **Phase 1 terminée** : compile, `go vet` clean, tous les tests passent.
- [x] **Phase 1bis terminée** : page de revue + restauration en place.
- [ ] Reste à faire : doc man, Phase 2 (robustesse), Phase 3 (UI plus riche : score sur chaque entrée, toggle par feed).

## Comment activer en local
```sh
export OLLAMA_ENABLED=1
export OLLAMA_MODEL=llama3.1:8b   # ou le modèle qui tourne sur ton instance
# Optionnel : commencer sans filtrage, juste tags + score
export OLLAMA_FILTER_THRESHOLD=0
# Plus tard, quand 50+ articles seront starred/lus :
# export OLLAMA_FILTER_THRESHOLD=40
```
Au prochain refresh d'un feed, les nouvelles entrées seront enrichies en arrière-plan.
