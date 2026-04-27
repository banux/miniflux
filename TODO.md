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
- [x] Documenter les variables d'env dans le man `miniflux.1`.

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

## Phase 2 — Robustesse / observabilité ✅
- [x] Logs structurés : durée d'inférence par entrée (tags + score), récap par batch (count, erreurs, durée), `rescraped` flag pour tracer le re-fetch.
- [x] Backoff/retry sur erreur Ollama (1 retry sur erreurs réseau / 5xx, pas de retry sur 4xx, abandon propre si le contexte est annulé pendant le backoff).
- [x] Tests unitaires : parsing des réponses Ollama, prompt builders (profil + excerpt + troncature), retry/abandon, respect du contexte annulé.
- [x] Test d'intégration léger avec un faux serveur HTTP simulant Ollama (déjà couvert via `httptest.Server`).

## Phase 3 — UI ✅
- [x] Afficher le score dans la liste d'articles (badge "AI 42%" dans `item_meta`, visible uniquement si `ollamaEnabled` et l'entrée est scorée).
- [x] Toggle par feed dans les settings (`disable_ollama` sur `feeds`, checkbox dans `edit_feed`, court-circuit dans `EnrichEntries`).
- [x] Compteur d'articles filtrés à côté du lien menu (« Filtered (N) »), via `CountOllamaFiltered` propagé par tous les handlers UI.

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

## Phase 4 — Endpoint MCP (Model Context Protocol)
Exposer Miniflux comme un serveur MCP afin qu'un assistant LLM puisse lire et agir sur les feeds via le protocole standard. Réutilise la clé API existante pour l'authentification — pas de nouveau système de tokens.

### Auth & transport
- [ ] Endpoint HTTP `POST /mcp` (JSON-RPC 2.0, transport Streamable HTTP de la spec MCP).
- [ ] Auth via header `X-Auth-Token` (clé API Miniflux), même middleware que `/v1/*`. Refus 401 sans clé valide.
- [ ] CORS aligné sur l'API REST.

### Tools exposés (v1, lecture + actions de base)
- [ ] `list_unread_entries(limit, offset)` — entrées non lues de l'utilisateur authentifié.
- [ ] `list_starred_entries(limit, offset)`.
- [ ] `search_entries(query, limit)`.
- [ ] `get_entry(id)` — détail + contenu.
- [ ] `mark_entry_read(id)` / `mark_entry_unread(id)`.
- [ ] `star_entry(id)` / `unstar_entry(id)`.
- [ ] `list_feeds` / `list_categories`.
- [ ] (Si scope étendu) `set_ollama_feedback(id, +1|-1|0)`.

### Spec MCP à respecter
- [ ] `initialize` : annoncer `serverInfo`, capabilities `tools.listChanged=false`.
- [ ] `tools/list` : décrire chaque outil avec son JSON Schema d'entrée.
- [ ] `tools/call` : exécuter et retourner `content` (TextContent).
- [ ] Erreurs JSON-RPC standard (-32601 méthode inconnue, -32602 params invalides).

### Tests
- [ ] Tests handler : initialize, tools/list, tools/call avec et sans clé API valide.
- [ ] Test d'intégration léger contre un client MCP en mémoire.

### Risques / points de vigilance
- Ne pas leak d'autres utilisateurs : tous les outils filtrent par `request.UserID(r)` issu du middleware.
- Pas de side-effects par défaut : les outils mutateurs (`mark_*`, `star_*`) doivent être explicitement appelés, jamais déclenchés par `initialize`.
- Streamable HTTP, pas de SSE persistant pour la v1 — chaque requête JSON-RPC est self-contained.
- Pagination obligatoire sur les listes (limit/offset) pour ne pas dump 10k articles dans la fenêtre LLM.

## Phase 5 — Chat intégré avec un agent qui utilise les outils MCP
Ajouter une page de chat dans Miniflux où l'utilisateur dialogue avec un agent LLM. L'agent appelle les outils du serveur MCP (Phase 4) pour répondre — résumer les non-lus, retrouver un article, marquer comme lu, etc. Dépend strictement de Phase 4.

### Backend
- [ ] Reuse du client Ollama existant (`internal/integration/ollama/client.go`) pour appeler le modèle conversationnel (ou un modèle dédié via `OLLAMA_CHAT_MODEL`).
- [ ] Nouveau package `internal/integration/agent/` :
  - [ ] Boucle agentique ReAct-style : LLM → tool_call → exécution MCP → observation → LLM, jusqu'à `stop` ou `max_steps` (défaut 8).
  - [ ] Exécution des tools en process (pas via HTTP MCP) en réutilisant les mêmes handlers, pour éviter le round-trip réseau ; le serveur MCP reste exposé pour les clients externes.
  - [ ] Garde-fou : timeout global par tour, budget de tokens estimé, rate-limit par utilisateur.
- [ ] Persistance conversations : table `chat_conversations` + `chat_messages` (rôle, contenu, tool_calls JSON, ts). Une conversation = un user_id.
- [ ] Routes UI :
  - [ ] `GET /chat` — liste des conversations.
  - [ ] `GET /chat/{id}` — vue conversation.
  - [ ] `POST /chat/{id}/messages` — nouveau message (streaming SSE pour la réponse).
  - [ ] `POST /chat` — créer une conversation.
  - [ ] `DELETE /chat/{id}`.

### Config
- [ ] `CHAT_ENABLED` (bool, défaut 0) — découplé d'`OLLAMA_ENABLED` parce que le scoring peut tourner sans le chat et inversement.
- [ ] `CHAT_MODEL` (string, défaut = `OLLAMA_MODEL`).
- [ ] `CHAT_MAX_STEPS` (int, défaut 8).
- [ ] `CHAT_TIMEOUT` (secondes, défaut 120).

### UI
- [ ] Page chat (template + JS minimal) avec affichage incrémental (SSE).
- [ ] Affichage des `tool_calls` (déroulant compact "the agent ran search_entries(query=...) → 5 results") pour la transparence.
- [ ] Lien dans le menu principal, gated par `chatEnabled`.
- [ ] Préserver markdown léger dans les réponses.

### Tests
- [ ] Tests boucle agentique : terminaison correcte, max_steps respecté, propagation d'erreur tool.
- [ ] Tests streaming handler avec httptest.
- [ ] Test des bornes user (pas de fuite cross-user via les tools).

### Risques / points de vigilance
- Boucle infinie : capper `max_steps` + watchdog timeout + détecter répétition d'appel identique.
- Coût : un chat par défaut peut faire 5–10 appels modèle ; bien afficher la progression à l'UI pour que l'attente soit lisible.
- Sécurité : les tools mutateurs doivent passer par une **confirmation user** côté UI avant exécution effective (ou whitelist explicite côté config), pour éviter qu'un prompt injection ne marque tout comme lu.
- Contexte LLM : injecter un préambule système qui rappelle « tu n'as accès qu'aux outils suivants, n'invente pas les IDs ».
- Politique de contenu : ne pas envoyer le contenu des articles privés au modèle si l'utilisateur a coché un futur `disable_chat` sur le feed.

## État courant
- [x] Branche `ollama` créée.
- [x] TODO.md rédigé.
- [x] **Phase 1 terminée** : compile, `go vet` clean, tous les tests passent.
- [x] **Phase 1bis terminée** : page de revue + restauration en place.
- [x] **Phase 2 terminée** : retry/backoff, logs structurés, tests prompt/retry/contexte.
- [x] **Phase 3 terminée** : badge score dans les listes, toggle par feed, compteur d'articles filtrés dans le menu.
- [ ] **Phase 4** : endpoint MCP avec auth par clé API (à démarrer).
- [ ] **Phase 5** : chat avec agent qui exploite les tools MCP (dépend de Phase 4).

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
