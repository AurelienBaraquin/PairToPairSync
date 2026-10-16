# Architecture

Un seul binaire Go (`cmd/p2psync`), sans CGO. Dépendance externe unique : `fsnotify`.

```
cmd/p2psync          CLI + démon (rechargement auto de la config)
internal/app         assemblage : config + identité + moteurs de dossiers + réseau + API
internal/node        TLS, connexions, routage des messages, dédoublonnage des connexions
internal/protocol    format de trame (en-tête JSON + corps binaire), messages
internal/folder      moteur « copie complète » : scan, watcher, index, téléchargement, conflits
internal/relay       moteur « relais » : changements en attente, acquittements, purge
internal/model       vecteurs de versions, FileInfo, blocs SHA-256
internal/identity    certificat ed25519 auto-signé, ID d'appareil = SHA-256 du certificat
internal/discovery   annonces UDP sur le réseau local
internal/api         page d'état locale (127.0.0.1)
mobile               API compatible gomobile
```

## Sécurité

TLS 1.3 avec authentification mutuelle. Il n'y a pas d'autorité de certification : l'identité d'un appareil
est le hachage de son certificat, comparé à la liste des appareils configurés (épinglage). Un appareil
inconnu est déconnecté avant tout échange. Tous les chemins reçus sont validés (pas de `..`, pas de chemin
absolu, pas de `.p2psync`), l'écriture refuse de traverser un lien symbolique, et chaque bloc reçu est
vérifié par son SHA-256.

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
(blocs adressés par hachage). Pour chaque changement : l'ensemble `Acked` des appareils qui possèdent la
version.

| Événement | Effet sur le relais |
|---|---|
| Un appareil complet annonce une version | entrée créée (ou remplacée si plus récente) ; l'émetteur est acquitté |
| Un appareil absent n'a pas acquitté | le relais **télécharge les blocs** depuis un appareil qui les a |
| Tous les appareils sont connectés | il attend (`HoldTime`) : ils se synchronisent directement, rien n'est stocké |
| Un appareil annonce la même version (il l'a reçue) | acquittement |
| Connexion d'un appareil / toutes les 30 s | **sonde** « avez-vous la version V de F ? » → acquitte les livraisons faites sans lui |
| Tous les appareils complets ont acquitté | entrée supprimée, puis **GC des blocs** non référencés |
| Quota atteint / rétention dépassée | pas de stockage / entrée jetée |

Côté appareil complet, un nouveau relais ne reçoit que les changements **futurs** (compteur `relay_sent`
confirmé par `IndexAck`, donc rien n'est perdu si la connexion tombe en route).
Les relais ne se parlent pas entre eux et ne comptent jamais comme destinataires finaux.

## Pistes d'évolution

Index différentiel entre appareils complets, purge des pierres tombales, détection de renommage,
traversée de NAT / découverte globale, application Android native, intégration shell (icônes de statut),
sélection de sous-dossiers, versionnage.
