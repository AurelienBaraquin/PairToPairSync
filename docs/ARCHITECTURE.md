# Architecture

Un seul binaire Go (`cmd/p2psync`), sans CGO. Réseau : [libp2p](https://libp2p.io). Autres dépendances :
`fsnotify` (surveillance de fichiers), `x/crypto` (XChaCha20-Poly1305, HKDF).

```
cmd/p2psync          CLI + démon (rechargement auto de la config, arrêt borné)
internal/app         assemblage : config + identité + moteurs de dossiers + réseau + API
internal/node        hôte libp2p, gater, sessions, flux par requête, supervision de connexion
internal/protocol    format de trame (en-tête JSON + corps binaire), messages, entrées scellées
internal/folder      moteur « copie complète » : scan, watcher, index, téléchargement reprenable, conflits, scellement
internal/relay       moteur « relais » : changements scellés en attente, acquittements, purge
internal/seal        chiffrement du relais : HKDF, HMAC, XChaCha20-Poly1305
internal/model       vecteurs de versions, FileInfo, blocs SHA-256
internal/pairing     invitations QR : format, jetons à usage unique, client d'appairage
internal/identity    clé Ed25519, ID d'appareil = peer ID libp2p
internal/api         page d'état locale (127.0.0.1, jeton, CSP) et création d'invitations
mobile               API gomobile (Pair, Start/Stop/Restart supervisés, NewInvite, ConfigJSON, Logs…)
android              application Kotlin/Compose autour du moteur (AAR), service au premier plan
```

## Réseau

- **Identité** : clé Ed25519 ; l'ID d'appareil est le peer ID libp2p dérivé de la clé publique. La
  connexion sécurisée (QUIC/TLS 1.3 ou TCP+Noise) prouve la possession de la clé : il n'y a pas de CA.
- **Gater** (`node/gater.go`) : `InterceptPeerDial`, `InterceptAddrDial`, `InterceptSecured` ne laissent
  passer que les appareils configurés. Un inconnu est coupé pendant la poignée de main.
- **Flux** : un flux long `session` par paire (Hello, Index, IndexAck, Ping) et un flux court par
  `block` ou `probe`. Sur QUIC ce sont des flux indépendants, donc un gros bloc ne retarde pas les pings.
- **Une seule session par paire** : si deux sessions s'ouvrent simultanément, celle ouverte par l'appareil
  de plus petit ID survit des deux côtés. Si la session passe par un relais de connexion et qu'une connexion
  directe apparaît (perçage réussi), la session est rouverte sur le chemin direct.
- **Joignabilité** : mDNS ; adresses de la configuration ; DHT Kademlia *privée* (préfixe `/p2psync`, et
  le gater empêche tout étranger de la rejoindre) ; NATPortMap ; AutoNAT v2 ; DCUtR ; AutoRelay + service de
  relais de circuit v2 sans limites de débit/durée **mais restreint par ACL à vos appareils**.
  Les candidats-relais d'AutoRelay sont vos propres appareils.
- **Supervision** : `dialLoop` par appareil, délai 1 s → 30 s avec gigue, réveillé par mDNS ou une connexion
  entrante ; pings toutes les 8 s, délai de lecture 30 s. `Stop()` borne dans le temps chaque composant tiers
  (un `Close` qui ne revient pas ne peut pas garder le démon en vie).

## Appairage par QR code (`internal/pairing`, `app/pair.go`, `node.servePair`)

1. `invite` (CLI ou page locale) lit les adresses du démon, génère un jeton aléatoire et écrit
   `invites/<sha256(jeton)>.json` (le jeton lui-même n'est pas stocké). Le texte `p2psync://pair/<base64url JSON>`
   (≈ 400 octets) est affiché en QR.
2. `pair` ouvre un hôte libp2p éphémère **avec la clé d'identité de l'invité**, se connecte à l'ID de
   l'invitation (libp2p refuse un autre ID : l'invitant est authentifié) et ouvre le flux
   `/p2psync/1.0/pair` avec `{token, name, port}`.
3. Côté invitant, le gater admet un inconnu **uniquement si** `invites/` contient une invitation non
   expirée (lecture mise en cache 1 s) et qu'il n'est pas banni. Il peut parler le seul flux `pair` (les
   flux session/block/probe exigent un pair connu ; le DHT, enveloppé par `gatedHost`, ne répond qu'aux
   appareils configurés). Limites : 6 inconnus simultanés dont 2 par IP, coupés après 15 s, un échange
   `pair` à la fois par appareil, requête lue avec `RecvMax(4096)` (la taille est vérifiée avant toute
   allocation).
4. `Consume` supprime le fichier du jeton : la suppression est atomique, un seul des appelants
   concurrents réussit (testé à 20 en parallèle). Les échecs sont comptés **par appareil** (3 → banni
   10 min) ; une identité qui échoue ne peut donc pas annuler les invitations des autres.
5. Si le jeton est valide, la réponse contient le nom, le dossier et — **seulement si l'invité est un
   appareil complet** — le secret. Le changement de configuration (`commit`) est exécuté 500 ms *après*
   l'envoi de la réponse, car il redémarre le démon.
6. L'invité applique `PreflightJoin` **avant** de dépenser le jeton (dossier requis, rôles compatibles),
   puis `Join`, qui ne fait pas confiance à l'invitant : la réponse doit porter sur le dossier invité, un
   appareil complet doit recevoir un secret (identique à celui qu'il possède déjà s'il a le dossier), un relais
   n'en reçoit jamais ; l'identifiant de dossier (`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`) devient un nom de
   fichier et est validé partout (`Decode`, `AddFolder`, `Validate`, `folder.New`).

Cas gérés : invitant complet → invité complet (secret transmis) ; invitant complet → invité relais
(pas de secret) ; invitant relais → invité complet (qui possède déjà le dossier et son secret). Un relais
ne peut pas inviter un relais.

## Application Android (`android/`, `mobile/`)

Le moteur Go est compilé en AAR par `gomobile bind` (arm64 seulement, segments alignés sur 16 Ko) et appelé via la classe générée
`mobile.Mobile`. Frontière volontairement étroite : chaînes JSON (`StatusJSON`, `ConfigJSON`) et entiers ; le
code Kotlin ne touche jamais aux clés ni au secret (la vue de configuration n'en contient pas, testé des deux
côtés). `Start` lance un superviseur qui redémarre le moteur quand `config.json` change (appairage, réglages)
ou sur `Restart()` (changement de réseau). Chaque fonction exportée récupère les paniques (le runtime Go vit dans
le processus de l'application : une panique non rattrapée la tuerait). Android restreignant l'énumération des
interfaces réseau, le service transmet les adresses de la connexion active (`LinkProperties`) via
`SetLocalAddrs`, que le nœud ajoute aux adresses annoncées (`Options.LocalIPs`). Côté Kotlin, `RelayService` (premier plan, `specialUse`) sérialise
démarrage/arrêt sur un thread unique, applique « Wi‑Fi uniquement » via un `NetworkCallback` avec
temporisation, et ne demande jamais l'état du moteur sur le thread principal. Les liens `p2psync://pair/…` ne
déclenchent qu'une boîte de confirmation (`InviteInfo` est lu côté Kotlin pour l'affichage ; la vraie
validation est celle du moteur Go).

## Modèle de données

Un fichier est décrit par `FileInfo` : nom, type, taille, date, mode, suppression, **vecteur de versions**
et liste des hachages SHA-256 de ses blocs (128 Ko, jusqu'à 16 Mo pour les très gros fichiers).
Comparaison de deux versions : `Equal`, `Greater`, `Lesser` ou `Concurrent`. Un transfert ne demande que les
blocs manquants ; les blocs déjà présents dans l'ancienne version locale sont copiés, pas retéléchargés.

## Cycle d'une modification (copie complète)

1. Le watcher signale un chemin ; après 300 ms de calme, le scanner le ré-indexe (re-hachage seulement si
   taille/date ont changé ; double `stat` pour ignorer un fichier en cours d'écriture). Les fichiers reçus
   reçoivent la date de l'émetteur, donc ne sont pas pris pour des modifications locales.
2. Si le contenu a changé, le vecteur est incrémenté et l'entrée est diffusée à tous les pairs connectés.
3. Le pair compare avec son index. Plus récent → à télécharger ; concurrent → même contenu : fusion
   silencieuse ; sinon un gagnant déterministe, le perdant devient un `*.sync-conflict-*`.
4. Le téléchargement écrit dans `.p2psync/tmp`, vérifie les blocs, revérifie que le fichier local n'a pas
   changé entre-temps (sinon il est mis de côté comme conflit), puis `rename` atomique.
5. Un marqueur `.p2psync/` protège contre un disque non monté : sans lui, le dossier se met en erreur au
   lieu de diffuser une suppression massive.

## Le relais

Un relais n'a pas d'arborescence. Son état est `pending.json` (changements en attente) + `blocks/`
(blocs chiffrés adressés par identifiant). Pour chaque changement : l'ensemble `Acked` des appareils qui
possèdent la version.

| Événement | Effet sur le relais |
|---|---|
| Un appareil complet annonce une version | entrée créée (ou remplacée si plus récente) ; l'émetteur est acquitté |
| Un appareil absent n'a pas acquitté | le relais **télécharge les blocs chiffrés** depuis un appareil qui les a |
| Tous les appareils sont connectés | il attend (`HoldTime`) : ils se synchronisent directement, rien n'est stocké |
| Un appareil annonce la même version (il l'a reçue) | acquittement |
| Connexion d'un appareil / toutes les 30 s | **sonde** « avez-vous la version V de F ? » → acquitte les livraisons faites sans lui |
| Tous les appareils complets ont acquitté | entrée supprimée, puis **GC des blocs** non référencés |
| Quota atteint / rétention dépassée | pas de stockage / entrée jetée |

Côté appareil complet, un nouveau relais ne reçoit que les changements **futurs** (compteur `relay_sent`
confirmé par `IndexAck`, donc rien n'est perdu si la connexion tombe en route). Les relais ne se parlent pas
entre eux et ne comptent jamais comme destinataires finaux.

### Protocole scellé (`internal/seal`, `folder/sealed.go`)

Clés dérivées du secret du dossier par HKDF-SHA256 (étiquettes `name`, `meta`, `block`, `blockid`).

| Donnée | Ce que voit le relais |
|---|---|
| chemin | `ID = HMAC(name)[:16]` (hex) — opaque, dépend du secret (pas d'attaque par dictionnaire) |
| métadonnées | `XChaCha20-Poly1305(nonce aléatoire, AAD = ID‖version)` : nom, mode, hachages en clair des blocs |
| bloc | `XChaCha20-Poly1305(nonce = HMAC(hash_en_clair), AAD = hash_en_clair)`, stocké sous `BlockID = HMAC(hash_en_clair)` |
| en clair | vecteur de versions, date, indicateur de suppression, taille, taille de bloc, nombre de blocs |

`BlockID` se calcule sans relire le fichier. Le destinataire rejoue tous les contrôles : il authentifie les
métadonnées, vérifie que les champs en clair du relais (version, taille, suppression, date, liste de blocs)
correspondent aux métadonnées authentifiées, déchiffre chaque bloc (l'AEAD échoue si le relais l'a modifié ou
remplacé) puis revérifie le SHA-256 du clair. Un relais refuse les entrées en clair, et un appareil complet
refuse de servir un bloc en clair à un relais.

## Pistes d'évolution

Index différentiel entre appareils complets, purge des pierres tombales, détection de renommage, échange
assisté et rotation du secret (QR code), application Android native, intégration shell (icônes de statut),
sélection de sous-dossiers, versionnage, validation sur de vrais réseaux (NAT, mDNS, UPnP).

## Robustesse face aux données reçues

Un pair de confiance peut être bogué ou compromis, et le relais est opaque : tout ce qui arrive du réseau est
validé (`validEntry` : tailles, nombre et taille des blocs, empreintes ; noms de fichiers, y compris les règles
de Windows). Les trames sont lues par morceaux (jamais d'allocation de la taille annoncée), avec des limites
propres à chaque message ; les goroutines par connexion, le scan et l'application d'un changement rattrapent
les paniques. Un changement reçu est **ré-évalué contre la version locale courante**, et une dernière fois
juste avant de remplacer le fichier : un téléchargement en file ne peut pas écraser une modification locale
plus récente. Les écritures d'état (index, `pending.json`, config, clé) sont atomiques et synchronisées ; un
index ou un fichier du relais endommagé est mis de côté (`.corrupt`) au lieu d'empêcher le démarrage.
Le relais n'acquitte un changement qu'après l'avoir écrit durablement.

## Windows

`internal/fsutil` regroupe ce qui diffère : renommage/suppression avec nouvelles tentatives sur les erreurs
transitoires (antivirus, indexeur, fichier ouvert), écriture atomique, attribut « masqué » pour `.p2psync`,
liste d'accès restreinte au compte courant pour le dossier de données. Les dossiers synchronisés ne peuvent
pas se chevaucher ni contenir le dossier de données (`config.CheckPaths`). Le démon (`p2psyncd.exe`, sous-système
graphique) ne montre aucune fenêtre ; `run --log` écrit son journal dans un fichier.
