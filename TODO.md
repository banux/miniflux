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

## Phase 4 — Endpoint MCP (Model Context Protocol) ✅
Exposer Miniflux comme un serveur MCP afin qu'un assistant LLM puisse lire et agir sur les feeds via le protocole standard. Réutilise la clé API existante pour l'authentification — pas de nouveau système de tokens.

### Auth & transport
- [x] Endpoint HTTP `POST /mcp` (JSON-RPC 2.0).
- [x] Auth via header `X-Auth-Token` (clé API Miniflux), middleware sister de `/v1/*`. Refus 401-équivalent JSON-RPC sans clé valide.
- [ ] CORS aligné sur l'API REST. *(pas critique pour la v1 — le serveur est consommé par des agents pas par un navigateur tier ; à ajouter si besoin)*

### Tools exposés (v1, lecture + actions de base)
- [x] `list_unread_entries(limit, offset)` — entrées non lues de l'utilisateur authentifié.
- [x] `list_starred_entries(limit, offset)`.
- [x] `search_entries(query, limit)`.
- [x] `get_entry(id)` — détail + contenu.
- [x] `mark_entry_read(id)` / `mark_entry_unread(id)`.
- [x] `toggle_starred(id)` (préféré à 2 tools `star`/`unstar` pour ne pas exposer un état que le LLM doit deviner).
- [x] `list_feeds` / `list_categories`.

### Spec MCP à respecter
- [x] `initialize` : `serverInfo` + capabilities `tools.listChanged=false`.
- [x] `tools/list` : chaque outil avec son JSON Schema d'entrée.
- [x] `tools/call` : retourne `content` (TextContent JSON-encodé pour les listes, "ok" pour les actions).
- [x] Erreurs JSON-RPC standard (-32700 parse, -32600 invalid req, -32601 méthode inconnue, -32602 params invalides).
- [x] Notifications (sans `id`) → 204 sans body.
- [x] `notifications/initialized` et `ping` gérés.

### Tests
- [x] Tests handler : initialize, tools/list, ping, tool inconnu, params invalides, notif → 204, refus sans clé API.
- [x] Garde-fou `TestRegisteredToolHandlersHaveASchema` : un outil enregistré sans schema (ou inversement) fait échouer la suite.

### Risques / points de vigilance
- Ne pas leak d'autres utilisateurs : chaque outil filtre par `request.UserID(r)` posé par le middleware.
- Projections LLM-friendly : on ne renvoie pas les modèles Miniflux complets (timestamps, scraper rules, flags) — `entrySummary` / `feedSummary` / `categorySummary` ne sortent que ce dont l'agent a besoin.
- Pagination obligatoire (defaultEntryLimit=25, max=100) pour ne pas dump 10k articles dans la fenêtre LLM.
- Pas de side-effects par défaut : les outils mutateurs (`mark_*`, `toggle_starred`) ne sont déclenchés que sur `tools/call` explicite.

## Phase 5 — Chat intégré avec un agent qui utilise les outils MCP ✅
Ajouter une page de chat dans Miniflux où l'utilisateur dialogue avec un agent LLM. L'agent appelle les outils du serveur MCP (Phase 4) pour répondre — résumer les non-lus, retrouver un article, marquer comme lu, etc.

### Backend
- [x] Réutilise le client Ollama existant (mêmes URL et modèle qu'`OLLAMA_MODEL`).
- [x] Nouveau package `internal/integration/agent/` : boucle ReAct LLM → tool_call → MCP → observation → LLM, capée par `CHAT_MAX_STEPS` (défaut 8). Toutes les étapes (user msg, tool calls, observations, réponse finale) sont persistées avant le redirect, donc l'utilisateur voit la transcription complète.
- [x] Round-trip via `mcp.CallTool` (in-process) avec un `*http.Request` synthétique portant `request.UserID` — l'agent traite MCP comme opaque, pas d'auth dance ni de réseau.
- [x] `internal/integration/ollama/agent_chat.go` : nouvelle méthode `ChatWithTools` qui parle `/api/chat` avec `tools[]` et parse `tool_calls`.
- [x] Persistance : tables `chat_conversations` + `chat_messages` avec `tool_calls jsonb`, `tool_name`, contrainte CHECK sur le role.
- [x] Routes UI : `GET /chat`, `POST /chat`, `GET /chat/{id}`, `POST /chat/{id}/messages`, `POST /chat/{id}/delete`.

### Config
- [x] `CHAT_ENABLED` (bool, défaut 0) — gate qui dépend aussi d'`OLLAMA_ENABLED`.
- [x] `CHAT_MAX_STEPS` (int, défaut 8, range 1–32).
- [x] `CHAT_TIMEOUT` (secondes, défaut 120).
- *(Décision : pas de `CHAT_MODEL` séparé pour l'instant, on reste sur `OLLAMA_MODEL`. À ajouter si l'expérience montre qu'un modèle tool-calling dédié est nécessaire.)*

### UI
- [x] Page liste `chat_list.html` + page conversation `chat_conversation.html`.
- [x] Affichage des `tool_calls` (avec arguments) et `tool_results` (dans un `<details>` pour ne pas polluer la lecture).
- [x] Lien dans le menu principal, gated par `chatEnabled`.
- [x] Stratégie batch (pas de SSE pour la v1) : on attend la réponse complète puis on rafraîchit la page.
- [x] Bouton de suppression de conversation (data-confirm).

### Tests
- [x] Tests des helpers purs : `deriveTitle`, `buildLLMMessages`, `convertToolCalls`, `buildToolCatalog`.
- [x] Tests existants `mcp` couvrent le dispatch, le `Catalog()` et `CallTool()` côté MCP.
- [x] Tests Ollama existants couvrent le retry, la troncature et le parsing.

### Risques / points de vigilance
- Boucle infinie : capée par `CHAT_MAX_STEPS` + `CHAT_TIMEOUT` global. Une exhaustion produit un assistant message explicite "Reached the N-step budget...".
- Coût : un chat peut faire 3-8 appels modèle ; pas de SSE en v1 → l'utilisateur attend. Acceptable pour la v1.
- Sécurité : les tools mutateurs (`mark_entry_read`, `toggle_starred`) sont en whitelist auto comme demandé. Le system prompt rappelle « never bulk-mutate, only act on entries the user explicitly asked about ».
- Contexte LLM : préambule système strict, regénéré à chaque tour.

## État courant
- [x] Branche `ollama` créée.
- [x] TODO.md rédigé.
- [x] **Phase 1 terminée** : compile, `go vet` clean, tous les tests passent.
- [x] **Phase 1bis terminée** : page de revue + restauration en place.
- [x] **Phase 2 terminée** : retry/backoff, logs structurés, tests prompt/retry/contexte.
- [x] **Phase 3 terminée** : badge score dans les listes, toggle par feed, compteur d'articles filtrés dans le menu.
- [x] **Phase 4 terminée** : endpoint MCP `POST /mcp` avec auth par clé API, 9 tools exposés, tests de dispatch.
- [x] **Phase 5 terminée** : chat avec agent ReAct qui exploite les tools MCP in-process, persistance des conversations, UI `/chat` avec liste + transcription + tool calls visibles.
- [ ] **Phase 6 en cours** : 6/13 chantiers livrés (markdown, rename, repeat-call, CHAT_MODEL, disable_chat, 3 nouveaux tools MCP, profil feedback). Reste : SSE+stop, history truncation, rate-limit, backoff inter-tour, bulk-mark, tests d'intégration.

## Phase 6 — v2 : streaming, robustesse agentique, profil enrichi
Phase de durcissement qui s'attaque aux limites laissées en v1, regroupées par thème.

### Streaming et expérience utilisateur du chat
- [ ] Endpoint SSE `GET /chat/{id}/stream` qui emet les events `assistant_partial`, `tool_call`, `tool_result`, `done`. Côté JS, abonnement au moment du POST + rendu incrémental.
  - **Pourquoi** : aujourd'hui un tour fait 3-8 appels modèle, l'utilisateur attend en aveugle. Première amélioration UX visible.
  - **Comment** : Ollama supporte `stream:true` sur `/api/chat`. Adapter `ChatWithTools` en `StreamChatWithTools(ctx, …) <-chan AgentEvent`. L'agent émet ses étapes au fur et à mesure ; le handler les forwarde au client.
- [ ] Bouton « Stop » côté UI : `POST /chat/{id}/cancel` qui annule le contexte de la run en cours (table d'in-flight runs `sync.Map[conv_id]context.CancelFunc`).
- [x] Rendu markdown léger des réponses assistant (goldmark → sanitizer existant).
- [x] Renommer une conversation dans l'UI (`POST /chat/{id}/rename` + `<details>` inline).
- [ ] Pagination ou troncature des messages : quand la transcription dépasse N tours, n'envoyer au LLM que le system prompt + les K derniers tours + un résumé des plus anciens.

### Robustesse agentique
- [x] Détection d'appel répété : si le modèle émet exactement le même `(tool_name, arguments)` deux fois de suite, abréger avec un message "the agent stopped after detecting a duplicate tool call". Hash sur `name + JSON-canonical args`.
- [ ] Backoff entre les tours sur erreur de tool (200 ms × 1.5^step) avant nouvel appel modèle, pour éviter qu'un Ollama instable consomme tout le budget de steps.
- [x] `CHAT_MODEL` séparé (défaut = `OLLAMA_MODEL`) pour pouvoir charger un modèle tool-calling dédié sans toucher au modèle de scoring.
- [x] `disable_chat` par feed (colonne `feeds.disable_chat`, checkbox dans `edit_feed`). MCP `list_feeds` skippe ces feeds, et `EntryQueryBuilder.HideChatDisabledFeeds()` est appliqué dans tous les tools qui retournent des entries (list_unread, list_starred, search_entries, get_feed_entries).
- [ ] Rate-limit par user : N runs simultanées max, M runs / heure. Renvoyer 429 plutôt que d'empiler.

### Tests d'intégration
- [ ] Test end-to-end de la boucle agentique avec httptest+sqlite (ou postgres-via-testcontainers selon la préférence du projet) : créer une conversation, simuler une réponse Ollama avec un tool_call, vérifier que le tool s'exécute et que le second tour reçoit l'observation.
- [ ] Test des bornes user via les tools : un utilisateur A ne doit jamais voir/modifier les entrées de B même via une fausse `tools/call`.
- [ ] Tests SSE : ordre des events, propre fermeture sur cancel.

### Outils MCP supplémentaires (v2)
- [x] `set_ollama_feedback(entry_id, value)` — bouton 👍/👎 exposé à l'agent.
- [x] `refresh_feed(feed_id)` — déclenche un fetch + enrichissement à la demande.
- [x] `get_feed_entries(feed_id, limit, offset)` — entries d'un feed précis.
- [ ] `bulk_mark_as_read({feed_id|category_id})` — gated par une politique de confirmation (la system prompt v1 interdit déjà les bulks ; ici on le rendrait explicite et confirmé).

### Propagation des feedbacks Ollama dans le profil ✅
- [x] `OllamaProfileSample.Feedback int` ajouté.
- [x] `GetOllamaUserProfile` : moitié du budget réservée aux entries `ollama_feedback != 0`, le reste rempli avec starred/read. Dédup par titre.
- [x] Prompt `ScoreEntry` reformaté en sections **Positive examples** / **Negative examples** avec markers `[thumbs-up] [starred] [read] [thumbs-down]`.
- [x] Tests prompt mis à jour pour la nouvelle structure.

### Risques / points de vigilance v2
- SSE et middleware d'auth : vérifier que `X-Auth-Token` peut être passé via le client EventSource (sinon basculer sur `fetch` + `ReadableStream`).
- Persistance partielle pendant un stream interrompu : on persiste à chaque `done` de tour, jamais sur des deltas, pour éviter d'écrire des messages tronqués.
- Bulk-mutating tools : restent gated par confirmation explicite côté UI ; ne jamais leur faire confiance par défaut malgré la whitelist.
- Cancel : doit faire fuiter zéro goroutine — toute run cancelable doit aussi nettoyer son entrée dans `inFlightRuns`.

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
