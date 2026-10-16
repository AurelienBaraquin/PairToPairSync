# Appareils administrateurs : se connecter à un seul appareil

Sans administrateur, chaque appareil doit être appairé avec chacun des autres (A↔B, A↔C, B↔C…). Avec un
**administrateur**, un nouvel appareil n'a qu'**une seule** connexion à faire : il se présente à l'admin, l'admin
accepte, et le nouvel appareil se retrouve relié aux autres appareils des dossiers que l'admin a choisi de
partager — sans que l'on touche aux autres appareils.

## En pratique

Sur l'appareil qui sera l'admin (un PC ou un Ubuntu/WSL qui détient les dossiers) :

```sh
p2psync admin enable                       # cet appareil devient admin
p2psync invite --network                   # QR code : tous les dossiers, copie complète
p2psync invite --network --folders docs,photos --as-relay   # téléphone : relais de ces dossiers seulement
p2psync invite --network --admin           # le nouvel appareil devient admin lui aussi
```

Sur le nouvel appareil :

```sh
p2psync pair <texte du QR> --dir D:\P2PSync     # Windows ;  --dir ~/P2PSync sous Ubuntu
```

(L'application Android scanne le QR directement.) La commande affiche qui invite, quels dossiers sont proposés
et où ils seront créés, puis demande confirmation. **Sur l'admin**, la demande apparaît :

```sh
p2psync requests          # nom, ID complet et adresse de l'appareil qui demande
p2psync accept <demande>  # ou: p2psync reject <demande>    (ou boutons sur la page locale : p2psync ui)
```

Une fois accepté, le nouvel appareil crée chaque dossier sous `--dir`, reçoit leurs secrets (copie complète) et
apprend l'existence des autres appareils de ces dossiers ainsi que **où les joindre**. Les autres appareils
l'apprennent de l'admin, la prochaine fois qu'ils se connectent à lui, et le nouvel appareil et eux se
connectent **directement**, même si l'admin est éteint. `--no-approve` supprime la confirmation (le QR seul
suffit alors) ; à éviter si le code peut avoir été vu par quelqu'un d'autre.

Autres commandes : `p2psync network` (qui est qui), `p2psync admin promote|demote <ID>` (faire confiance à un
appareil déjà appairé comme admin), et `p2psync device remove <ID>` sur l'admin **révoque** l'appareil : les
autres le retirent d'eux-mêmes.

## Ce que l'admin peut et ne peut pas faire

Un admin signe, avec la clé de son appareil, des **autorisations** (« l'appareil S participe aux dossiers F, en
copie complète ou en relais »). Chaque appareil les vérifie lui-même ; il n'y a ni serveur ni autorité.

| L'admin peut | L'admin ne peut pas |
|---|---|
| présenter un appareil aux membres des dossiers dont **il est lui-même membre** | toucher un dossier dont il n'est pas membre |
| choisir, par invitation, quels dossiers sont partagés et si le nouvel appareil est copie complète / relais / admin | voir ou donner accès aux dossiers qu'il n'a pas |
| révoquer un appareil qu'il a présenté | effacer des fichiers : retirer un dossier de la configuration ne supprime jamais les fichiers |
| promouvoir un autre admin | **faire entrer une copie complète s'il n'est qu'un relais** |

Ce dernier point protège le relais aveugle : un téléphone (qui n'a jamais le secret des dossiers) ne peut donc
pas être administrateur de copies complètes. Il peut, en revanche, ne présenter que des relais. L'application
Android rejoint un réseau, mais ne crée pas d'invitations d'admin.

**À qui faire confiance.** Vous ne faites confiance qu'aux admins que **vous** avez choisis : celui dont vous avez
scanné le code (il est affiché avant que vous confirmiez), et ceux qu'un admin de confiance a promus. Un admin
compromis peut introduire de nouveaux appareils dans les dossiers qu'il partage avec vous, et ces appareils
recevront alors le secret et les fichiers : traitez un admin comme un appareil complet de confiance. Pour
retirer un admin, supprimez-le (`device remove`) sur chacun de vos appareils.

## Ce qui est écrit où

- `config.json` : `admin` (cet appareil est admin), `devices[].admin` (admin de confiance, choisi par vous),
  `devices[].managed` et `folders[].managed` (entrées ajoutées par une autorisation, donc retirables par une
  autre), `network_dir` (où créer les dossiers reçus), `dismissed` (ce que vous avez retiré à la main).
  **Ce que vous avez créé à la main n'est jamais modifié ni supprimé par une autorisation.**
- `grants.json` : les autorisations signées reçues ou émises. Perdu ou corrompu, il est reconstruit par les autres
  appareils sans rien enlever à la configuration.
- Un dossier reçu d'un admin attend son secret (`awaiting_secret`) tant qu'aucun appareil complet connecté ne
  le lui a remis ; il démarre ensuite tout seul.

## Limites connues

- Un changement d'appartenance redémarre le démon des appareils concernés (comme tout changement de
  configuration) ; les connexions reviennent en quelques secondes.
- Un appareil apprend un nouveau venu quand il se **reconnecte** à un appareil qui le sait : si l'admin est
  éteint, les autres l'apprennent dès qu'il revient (ou par n'importe quel appareil qui le sait déjà).
- Si un appareil est retiré à la main alors qu'une autorisation le lui accorde encore, il revient seulement avec
  une autorisation plus récente.
- Les horloges des admins servent à ordonner les autorisations ; une horloge fausse de plus de 7 jours est refusée.
