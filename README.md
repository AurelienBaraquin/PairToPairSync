# PairToPairSync

Synchronisation de dossiers **en temps réel, de pair à pair et chiffrée** (dans l'esprit de Syncthing),
avec une fonctionnalité en plus : les **points de relais temporaires**.

Un relais (par exemple un téléphone) sert de boîte aux lettres : il reçoit les dernières modifications
quand il est joignable, les remet aux appareils qui étaient absents, puis **les efface**. Il ne contient
jamais une copie du dossier et n'est pas un stockage permanent.

```
   PC (maison)  ──────┐                         ┌────── Portable (bureau)
   éteint le soir     ├──►  📱 Téléphone  ◄─────┤       allumé le lendemain
                      │     (relais)            │
   modifie un fichier ┘     garde le fichier     └── le reçoit, le relais l'efface
```

## Un dossier comme les autres

Le dossier synchronisé est **un vrai dossier sur le disque** : il s'ouvre dans l'Explorateur Windows,
le Finder ou n'importe quel gestionnaire de fichiers, fonctionne hors-ligne et avec tous vos logiciels.
Un petit démon en arrière-plan surveille les changements (inotify / FSEvents / ReadDirectoryChangesW)
et synchronise en une fraction de seconde. Il n'y a aucun lecteur virtuel ni format propriétaire.

## Démarrage rapide

```sh
go build -o p2psync ./cmd/p2psync       # ou: make build   (binaire unique, sans dépendance)
```

Sur **chaque** appareil :

```sh
p2psync init --name portable            # crée l'identité ; affiche l'ID de l'appareil
```

Échangez les ID, puis déclarez les autres appareils et le dossier à partager (même `--id` partout) :

```sh
# Sur le PC (A) : B = portable (copie complète), T = téléphone (relais)
p2psync device add <ID_B> --name portable  --addr 192.168.1.20:22000
p2psync device add <ID_T> --name telephone --addr 192.168.1.30:22000
p2psync folder add ~/Documents/Projets --id projets --device <ID_B> --device <ID_T>:relay

# Sur le portable : symétrique
p2psync device add <ID_A> --name pc ...  ;  p2psync device add <ID_T> --name telephone ...
p2psync folder add ~/Projets --id projets --device <ID_A> --device <ID_T>:relay

# Sur le téléphone (relais) : il ne partage que les appareils « complets »
p2psync device add <ID_A> --name pc ... ;  p2psync device add <ID_B> --name portable ...
p2psync folder add-relay ~/relais --id projets --device <ID_A> --device <ID_B> --quota-mb 2048

p2psync run                              # sur chaque appareil (voir packaging/ pour le lancer au démarrage)
p2psync status                           # ou ouvrir http://127.0.0.1:8384
```

Le démon recharge `config.json` tout seul : pas besoin de le redémarrer après un `device add`.
Les appareils s'authentifient mutuellement par l'empreinte de leur certificat : **un appareil absent de
la configuration est refusé**. Sur un même réseau local, les adresses sont trouvées automatiquement
(`--addr` devient facultatif).

## Comment fonctionne le relais

- Il ne conserve que **la dernière version en attente** de chaque fichier (et les suppressions).
- Chaque modification est « acquittée » par les appareils qui la possèdent. Dès que **tous** les
  appareils complets l'ont, le relais supprime l'entrée **et les blocs de données** correspondants.
- Si tous les appareils sont en ligne, ils se synchronisent directement et le relais **ne télécharge
  rien** (il n'épuise ni la batterie ni le forfait du téléphone). Il ne stocke les données que
  lorsqu'au moins un appareil est absent.
- Garde-fous : un **quota** de stockage (`--quota-mb`) et une **durée de rétention** maximale
  (`--retention-hours`, 30 jours par défaut) après laquelle une modification jamais récupérée est jetée.
- Les modifications concurrentes ne perdent jamais de données (voir *Conflits*).

## Conflits

Chaque fichier porte un vecteur de versions. Si deux appareils modifient le même fichier hors-ligne,
une version gagne (la plus récente, de façon identique sur tous les appareils) et l'autre est conservée
à côté : `rapport.sync-conflict-20261016-101500-ABC123.docx`. Une modification l'emporte toujours sur
une suppression. Deux fichiers identiques créés des deux côtés ne produisent pas de conflit.

## Ignorer des fichiers

Fichier `.p2pignore` à la racine du dossier (`*.tmp`, `build/`, `/brouillons/*`).

## Limites actuelles (honnêtes)

- **Pas de traversée de NAT ni de découverte globale** : les appareils se joignent sur le réseau local,
  par VPN (Tailscale, WireGuard…), ou via une adresse/port joignable. Un relais sur un serveur public
  (VPS) ou sur un téléphone joignable résout le cas « jamais en ligne en même temps ».
- **Téléphone** : le binaire tourne tel quel sur Android via Termux (`GOOS=linux GOARCH=arm64`). Le paquet
  `mobile/` est prêt pour `gomobile bind` ; l'application Android native (service au premier plan,
  interface) reste à écrire.
- L'index complet est échangé à chaque connexion entre appareils complets (métadonnées seulement) ; les
  suppressions (« pierres tombales ») ne sont jamais purgées ; pas de détection de renommage (un renommage
  = suppression + création, mais les blocs identiques ne sont pas retransférés s'ils existent déjà dans le
  fichier précédent) ; liens symboliques ignorés ; pas de corbeille/versionnage ; seuls les bits de
  permission sont propagés.
- Testé sous Linux (tests de bout en bout avec `-race`). Windows/macOS/ARM sont compilés en CI mais pas encore testés à l'exécution.

Détails techniques : [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## Développement

```sh
make test      # go test ./... -race
make dist      # binaires pour Linux/Windows/macOS (amd64/arm64)
```
