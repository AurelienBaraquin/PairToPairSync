# PairToPairSync

Synchronisation de dossiers **en temps réel, de pair à pair, sans aucun serveur, entièrement chiffrée**
(dans l'esprit de Syncthing), avec une fonctionnalité en plus : les **points de relais temporaires**.

Un relais (par exemple un téléphone) sert de boîte aux lettres : il reçoit les dernières modifications
quand il est joignable, les remet aux appareils qui étaient absents, puis **les efface**. Il ne contient
jamais une copie du dossier, n'est pas un stockage permanent, et **ne peut rien lire** de ce qu'il porte.

```
   PC (maison)  ──────┐                         ┌────── Portable (bureau)
   éteint le soir     ├──►  📱 Téléphone  ◄─────┤       allumé le lendemain
                      │     (relais aveugle)    │
   modifie un fichier ┘     garde un blob chiffré └── le reçoit, le relais l'efface
```

## Plateformes

| Plateforme | Rôle | Livré |
|---|---|---|
| **Windows x64** | appareil complet (copie du dossier) | `p2psync.exe` (ligne de commande) et `p2psyncd.exe` (démon sans fenêtre) : `make windows` |
| **Ubuntu x64 (aussi dans WSL)** | appareil complet | `make linux`, voir [docs/WSL.md](docs/WSL.md) |
| **Android (arm64)** | relais temporaire | application dans [`android/`](android/README.md) |

Le code est écrit en Go portable et ses tests tournent sous Linux (c'est là qu'ils sont exécutés ici),
mais seuls Windows x64 et Android arm64 sont des cibles livrées et maintenues.

## Un dossier comme les autres

Le dossier synchronisé est **un vrai dossier sur le disque** : il s'ouvre dans l'Explorateur de fichiers
et fonctionne hors-ligne avec tous vos logiciels. Un démon en arrière-plan surveille les changements
(ReadDirectoryChangesW sous Windows) et synchronise en une fraction de seconde. Pas de lecteur virtuel,
pas de format propriétaire. Le dossier de service `.p2psync` (marqueur, téléchargements en cours) est
masqué. Les noms de fichiers que Windows refuse (`CON`, `aux.txt`, `:`, `?`, point ou espace final…) sont
signalés et non synchronisés plutôt que mal écrits ; deux noms qui ne diffèrent que par la casse sont
traités comme conflit sur un disque Windows.

## Démarrage rapide (avec QR code)

```sh
make windows                             # dist\p2psync.exe et dist\p2psyncd.exe (ou: go build ./cmd/p2psync)

# Sur le premier appareil (le PC) : créer le dossier et lancer le démon
p2psync init --name pc
p2psync folder add "C:\Users\moi\Documents\Projets" --id projets
p2psync run                              # en continu ; --log fichier.log pour écrire le journal dans un fichier

# Démarrage automatique à l'ouverture de session (tâche planifiée, sans fenêtre) :
#   powershell -ExecutionPolicy Bypass -File packaging\windows-autostart.ps1 -Exe C:\Outils\p2psyncd.exe

# Inviter le portable (copie complète) : un QR code s'affiche
p2psync invite
#   Sur le portable :   p2psync pair <texte du QR> --path "D:\Projets"

# Inviter le téléphone comme relais temporaire
p2psync invite --as-relay
#   Application Android : « Scanner un QR code »

# Pour que le portable aussi passe par le téléphone, le téléphone l'invite à son tour
#   (application, « Inviter un appareil »)     ->     (portable) p2psync pair <texte du QR>
```

Le QR code est aussi disponible dans la page d'état locale : `p2psync ui` affiche son lien (il contient un
jeton d'accès propre à votre session, lu dans un fichier `0600` — un autre compte de la machine ne peut
ni lire l'état ni créer d'invitation), bouton « Inviter un appareil ». L'application Android scanne ce QR directement (ou ouvre un lien `p2psync://pair/…`, toujours après
confirmation). Ports par défaut : 22100 (UDP+TCP) pour la synchronisation, 127.0.0.1:8484 pour la page locale.

**Ce que fait un scan.** L'invitation contient l'ID de l'appareil invitant, de quoi le joindre et un
**jeton aléatoire de 128 bits, valable 10 minutes, utilisable une seule fois**. L'invité se connecte à
l'invitant (dont l'identité est vérifiée par sa clé : personne ne peut se faire passer pour lui), présente
le jeton, et les deux appareils s'ajoutent mutuellement. Si l'invité est un appareil complet, le **secret
du dossier** lui est remis dans ce canal chiffré ; si c'est un relais, il ne le reçoit **jamais**. Rien
n'est à recopier à la main. Traitez le QR comme un mot de passe : quiconque le voit avant son expiration
peut rejoindre le dossier. Un appareil qui présente 3 jetons erronés est
refusé pendant 10 minutes, sans pouvoir gêner les autres invités.

Le démon recharge `config.json` tout seul. Si `config.json` devient illisible, le démon
garde la configuration qui tournait et le dit dans le journal. Les commandes manuelles (`device add`,
`folder add --secret-file`, `folder add-relay`…) restent disponibles : voir `p2psync` sans argument.

## Appareils administrateurs

Plutôt que d'appairer chaque appareil avec tous les autres, on peut désigner des **admins** : un nouvel appareil
scanne le QR d'un seul admin, l'admin accepte la demande, et le nouvel appareil est automatiquement relié aux
autres appareils des dossiers que l'admin a choisi de partager (qui l'apprennent de l'admin, puis se connectent
directement). `p2psync admin enable`, `p2psync invite --network`, `p2psync pair … --dir …`, `p2psync requests` /
`accept`. Un admin ne peut agir que sur les dossiers dont il est membre, un relais (téléphone) ne peut jamais faire
entrer une copie complète, et ce que vous avez configuré à la main n'est jamais modifié. Détails, limites et
modèle de confiance : [docs/ADMIN.md](docs/ADMIN.md).

## Sécurité

| Couche | Protection |
|---|---|
| Identité | Clé Ed25519 par appareil ; l'ID de l'appareil **est** sa clé publique. Pas d'autorité, pas de serveur. |
| Transport | QUIC (TLS 1.3) ou TCP+Noise, authentifiés mutuellement par ces clés, confidentialité persistante. |
| Contrôle d'accès | Un appareil **absent de la configuration est refusé pendant la poignée de main**, avant tout échange. |
| Intégrité | Chaque bloc de fichier est vérifié par SHA-256 ; les chemins reçus sont validés (pas de `..`, pas de liens symboliques suivis). |
| Relais | **Chiffré de bout en bout, à connaissance nulle** (ci-dessous). |
| Appairage | Invitation à usage unique (128 bits, 10 min). Les inconnus ne sont acceptés **que pendant une invitation**, 6 au plus (2 par adresse IP), 15 s au plus, un seul échange à la fois, et ne peuvent faire qu'une chose : présenter le jeton (requête limitée à 4 Ko). L'annuaire DHT leur est fermé. |
| Contenu venu d'un autre appareil | Identifiants de dossier, noms et libellés sont validés/filtrés (pas de `/`, `..`, ni balisage) ; un invité vérifie la réponse de l'invitant avant d'appliquer quoi que ce soit. |
| Réseau d'admins | Autorisations signées (Ed25519, séparation de domaine), ordonnées par numéro de séquence, vérifiées par chaque appareil ; seuls les admins que **vous** avez choisis (ou qu'ils ont promus) comptent ; un admin ne touche qu'aux dossiers dont il est membre ; chaque demande de connexion est confirmée par un humain côté admin ; les secrets de dossier ne sont remis qu'à une copie complète listée, jamais à un relais. |
| Page locale | `127.0.0.1` seulement ; jeton d'accès obligatoire ; refuse tout `Host` non local (anti DNS-rebinding) ; création d'invitation = POST JSON de même origine ; aucun script en ligne, texte inséré via `textContent`, politique CSP stricte. |
| Fichiers de config | `config.json`, clé, index : permissions `0600` ; sous Windows, le dossier de données (`%LOCALAPPDATA%\p2psync`) n'est accessible qu'à votre compte (liste d'accès non héritée). Écritures atomiques et synchronisées sur disque. |

**Le relais est aveugle.** Tous les appareils *complets* d'un dossier partagent un secret de 256 bits que
le relais n'a jamais (la configuration d'un relais refuse même d'en contenir un). Ce qui est remis au
relais : noms de fichiers → identifiants opaques (HMAC) ; métadonnées → XChaCha20-Poly1305, liées au chemin
et à la version (le relais ne peut ni les rejouer ni les déplacer) ; blocs de données → XChaCha20-Poly1305.
Les appareils authentifient tout ce qui vient du relais : un relais malveillant ou un disque corrompu ne
peut pas injecter ou modifier un fichier (testé). Un téléphone perdu ou saisi révèle seulement : tailles
approximatives, dates, compteurs de version, et que deux blocs sont identiques.

**Ce que le chiffrement ne couvre pas.** Le dossier synchronisé lui-même est un dossier normal, en clair sur
vos disques : activez le chiffrement du disque (BitLocker). Un appareil *complet*
compromis voit tout (il est de confiance par définition). La découverte sur le réseau local (mDNS) annonce
la présence de l'ID de votre appareil ; désactivez-la avec `"lan_discovery": false` si cela vous gêne.
Le secret du dossier se trouve dans `config.json` des appareils complets : protégez ce fichier.

## Connectivité sans serveur

Aucun serveur tiers n'est jamais contacté. Les appareils se trouvent et s'entraident :

1. **Réseau local** : découverte mDNS.
2. **Adresses mémorisées** et **annuaire privé (DHT)** réservé à vos appareils : si l'adresse d'un appareil
   change, n'importe lequel de vos autres appareils connectés peut la communiquer.
3. **Box / routeur** : ouverture automatique de port (UPnP, NAT-PMP, PCP) et détection de joignabilité (AutoNAT).
4. **Perçage de NAT** (DCUtR) entre deux appareils derrière des box différentes.
5. **Relais de connexion par vos propres appareils** : si deux appareils ne peuvent pas se joindre, un
   troisième à vous qui est joignable (PC avec port ouvert, IPv6, VPS que vous possédez…) fait transiter le
   flux **déjà chiffré de bout en bout** (il ne peut pas le lire). Seuls vos appareils configurés peuvent
   l'utiliser. Désactivable : `"relay_service": false`.

> Limite physique : si **aucun** de vos appareils n'est joignable de l'extérieur (tous derrière un NAT
> strict / CGNAT, sans IPv6), deux appareils ne peuvent pas s'atteindre sans aucune aide extérieure. Il
> suffit d'un seul appareil joignable (ou d'un VPN personnel type WireGuard/Tailscale) pour tout débloquer.

**Réseaux instables.** Reconnexion automatique avec délai exponentiel et gigue ; un lien mort est détecté en
moins de 30 s (pings applicatifs) ; chaque bloc voyage dans son propre flux, donc un transfert bloqué ne gèle
pas le reste ; les téléchargements sont **reprenables** (un fichier partiel est conservé et ses blocs
vérifiés puis réutilisés : après une coupure, l'appareil ne retélécharge que ce qui manque) ; chaque bloc est
retenté 3 fois ; le passage d'un chemin relayé à une connexion directe se fait tout seul.

## Comment fonctionne le relais

- Il ne conserve que **la dernière version en attente** de chaque fichier (et les suppressions).
- Chaque modification est « acquittée » par les appareils qui la possèdent. Dès que **tous** les
  appareils complets l'ont, le relais supprime l'entrée **et les blocs** correspondants.
- Si tous les appareils sont en ligne, ils se synchronisent directement et le relais **ne télécharge
  rien** (ni batterie ni forfait gaspillés). Il ne stocke que lorsqu'un appareil est absent.
- Garde-fous : **quota** de stockage (`--quota-mb`) et **rétention** maximale (`--retention-hours`,
  30 jours par défaut) après laquelle une modification jamais récupérée est jetée.

## Conflits

Chaque fichier porte un vecteur de versions. Si deux appareils modifient le même fichier hors-ligne, une
version gagne (de façon identique sur tous les appareils) et l'autre est conservée à côté :
`rapport.sync-conflict-20261016-101500-ABC123.docx`. Une modification l'emporte toujours sur une suppression.
Deux fichiers identiques créés des deux côtés ne produisent pas de conflit.

## Ignorer des fichiers

Fichier `.p2pignore` à la racine du dossier (`*.tmp`, `build/`, `/brouillons/*`).

## Limites actuelles (honnêtes)

- **Ce qui est testé** : tests de bout en bout sous Linux avec `-race` (synchro directe, conflits, relais
  aveugle, falsification, reprise après coupure, connexion à travers un appareil-relais, annuaire DHT,
  quota, appareil inconnu, données hostiles d'un pair, index corrompu, relais qui redémarre). Un audit
  indépendant (4 relectures + `govulncheck` + `staticcheck`) a été mené et ses constats vérifiés puis corrigés.
- **Windows n'a jamais été exécuté par moi.** Le code propre à Windows (renommage avec nouvelles tentatives
  sur « fichier utilisé », liste d'accès, attribut masqué, noms insensibles à la casse) compile et passe
  `go vet`, et la CI exécute les tests sur un runner `windows-latest` : c'est cette exécution, pas ce texte,
  qui en fait foi. Je n'ai pas pu tester non plus l'Explorateur, les antivirus ni les fichiers OneDrive.
- **Non testé en conditions réelles** : mDNS, UPnP/NAT-PMP et le perçage de NAT entre vraies box. Ce sont
  les fonctions de libp2p, mais je n'ai pas pu les valider sur de vrais réseaux.
- **Téléphone** : une **application Android native** est fournie dans [`android/`](android/README.md)
  (Kotlin/Compose, scan de QR code, service au premier plan, mode Wi‑Fi uniquement, arm64). Elle compile (APK,
  tests JVM, lint, bibliothèque alignée pour les pages mémoire de 16 Ko) mais **n'a jamais été exécutée sur un
  téléphone ni un émulateur** : à valider sur appareil, en particulier la caméra, le service au premier plan
  et l'économie d'énergie constructeur (Doze).
- L'index complet est échangé à chaque connexion entre appareils complets ; les suppressions ne sont
  jamais purgées ; pas de détection de renommage (retransfert complet) ; liens symboliques ignorés ; pas de
  corbeille/versionnage ; seuls les bits de permission sont propagés.
- Pas de rotation du secret du dossier.
- Un appairage est bilatéral (deux appareils) : relier trois appareils demande plusieurs invitations
  (A↔B, A↔relais, B↔relais), comme dans l'exemple ci-dessus.

Détails techniques : [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## Développement

```sh
make test      # go test ./... -race     (Go 1.26.9, versions épinglées dans go.mod)
make check     # gofmt + go vet (Linux et Windows) + tests
make windows   # dist/p2psync.exe + dist/p2psyncd.exe
make android   # bibliothèque Go (gomobile) + tests JVM + lint + APK de debug
```
