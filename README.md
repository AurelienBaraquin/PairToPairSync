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

## Un dossier comme les autres

Le dossier synchronisé est **un vrai dossier sur le disque** : il s'ouvre dans l'Explorateur Windows,
le Finder ou n'importe quel gestionnaire de fichiers, fonctionne hors-ligne et avec tous vos logiciels.
Un démon en arrière-plan surveille les changements (inotify / FSEvents / ReadDirectoryChangesW) et
synchronise en une fraction de seconde. Pas de lecteur virtuel, pas de format propriétaire.

## Démarrage rapide

```sh
go build -o p2psync ./cmd/p2psync        # ou: make build   (binaire unique)
p2psync init --name portable             # sur CHAQUE appareil : crée la clé d'identité
p2psync id                               # l'ID de cet appareil (clé publique)
```

Les appareils se déclarent mutuellement. Le plus simple : coller **une seule adresse** par appareil
(elle contient l'ID *et* un moyen de le joindre ; `p2psync status` les affiche) :

```sh
# PC : le portable est une copie complète, le téléphone un relais
p2psync device add /ip4/192.168.1.20/udp/22000/quic-v1/p2p/<ID_PORTABLE> --name portable
p2psync device add /ip4/192.168.1.30/udp/22000/quic-v1/p2p/<ID_TEL>      --name telephone
p2psync folder add ~/Documents/Projets --id projets --device <ID_PORTABLE> --device <ID_TEL>:relay
#   -> affiche le SECRET du dossier. À donner uniquement à vos autres appareils complets.

# Portable : même dossier, même secret
p2psync device add <adresse-du-PC> --name pc ;  p2psync device add <adresse-du-tel> --name telephone
p2psync folder add ~/Projets --id projets --secret <SECRET> --device <ID_PC> --device <ID_TEL>:relay

# Téléphone (relais) : il ne reçoit JAMAIS le secret
p2psync device add <adresse-du-PC> --name pc ;  p2psync device add <adresse-du-portable> --name portable
p2psync folder add-relay ~/relais --id projets --device <ID_PC> --device <ID_PORTABLE> --quota-mb 2048

p2psync run        # sur chaque appareil (voir packaging/ pour le lancer au démarrage)
p2psync status     # ou http://127.0.0.1:8384
```

Le démon recharge `config.json` tout seul. Après la première connexion, les adresses sont mémorisées et
retrouvées automatiquement (voir *Connectivité*).

## Sécurité

| Couche | Protection |
|---|---|
| Identité | Clé Ed25519 par appareil ; l'ID de l'appareil **est** sa clé publique. Pas d'autorité, pas de serveur. |
| Transport | QUIC (TLS 1.3) ou TCP+Noise, authentifiés mutuellement par ces clés, confidentialité persistante. |
| Contrôle d'accès | Un appareil **absent de la configuration est refusé pendant la poignée de main**, avant tout échange. |
| Intégrité | Chaque bloc de fichier est vérifié par SHA-256 ; les chemins reçus sont validés (pas de `..`, pas de liens symboliques suivis). |
| Relais | **Chiffré de bout en bout, à connaissance nulle** (ci-dessous). |
| Fichiers de config | `config.json`, clé, index : permissions `0600`. |

**Le relais est aveugle.** Tous les appareils *complets* d'un dossier partagent un secret de 256 bits que
le relais n'a jamais (la configuration d'un relais refuse même d'en contenir un). Ce qui est remis au
relais : noms de fichiers → identifiants opaques (HMAC) ; métadonnées → XChaCha20-Poly1305, liées au chemin
et à la version (le relais ne peut ni les rejouer ni les déplacer) ; blocs de données → XChaCha20-Poly1305.
Les appareils authentifient tout ce qui vient du relais : un relais malveillant ou un disque corrompu ne
peut pas injecter ou modifier un fichier (testé). Un téléphone perdu ou saisi révèle seulement : tailles
approximatives, dates, compteurs de version, et que deux blocs sont identiques.

**Ce que le chiffrement ne couvre pas.** Le dossier synchronisé lui-même est un dossier normal, en clair sur
vos disques : activez le chiffrement du disque (BitLocker, FileVault, LUKS). Un appareil *complet*
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
  quota, appareil inconnu). **Non testé en conditions réelles** : mDNS (ne fonctionne pas dans mon
  environnement de test), UPnP/NAT-PMP, et le perçage de NAT entre vraies box — ce sont les fonctions
  éprouvées de libp2p, mais je n'ai pas pu les valider sur de vrais réseaux. Windows/macOS/ARM sont
  compilés en CI mais pas exécutés.
- **Téléphone** : `make dist` produit `p2psync-android-arm64` (et `linux-arm64`) à lancer via Termux ; la
  compilation Android exige `-ldflags=-checklinkname=0` (déjà dans le Makefile). **Jamais exécuté sur un vrai
  téléphone.** Le paquet `mobile/` est prêt pour `gomobile bind` ; l'application Android native (service au
  premier plan, économie d'énergie) reste à écrire.
- L'index complet est échangé à chaque connexion entre appareils complets ; les suppressions ne sont
  jamais purgées ; pas de détection de renommage (retransfert complet) ; liens symboliques ignorés ; pas de
  corbeille/versionnage ; seuls les bits de permission sont propagés.
- Le secret du dossier se copie à la main entre appareils complets (ou `--secret`) ; il n'y a pas encore
  d'échange assisté (QR code) ni de rotation de secret.

Détails techniques : [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## Développement

```sh
make test      # go test ./... -race     (Go 1.26)
make dist      # binaires pour Linux/Windows/macOS (amd64/arm64)
```
