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
| choisir, par invitation, quels dossiers sont partagés et si le nouvel appareil est copie complète / relais / admin | se porter garant de lui-même, ni se déclarer « copie complète » : seul le propriétaire de **votre** appareil décide qui détient des copies complètes |
| révoquer ce qu'il a lui-même accordé | effacer des fichiers : retirer un dossier de la configuration ne supprime jamais les fichiers |
| promouvoir un autre admin | **faire entrer une copie complète s'il n'est qu'un relais** |

Chaque admin parle pour lui seul : ce qu'un appareil reçoit est la **réunion** de ce que disent les admins de
confiance. Un admin qui restreint ou retire son autorisation n'efface donc pas celle d'un autre, et un admin promu
ne peut pas couper celui qui l'a promu (l'inverse est vrai : retirer le droit d'admin à un admin retire aussi ceux
qu'il avait promus).

Ce dernier point protège le relais aveugle : un téléphone (qui n'a jamais le secret des dossiers) ne peut donc
pas faire entrer de copie complète. Pour qu'un **nouveau dossier** soit créé en copie complète sur la parole d'un
admin, celui-ci doit être un appareil que vous avez vous-même reconnu comme détenteur de copies complètes (vous
avez scanné son code pour une copie complète, ou il figure comme membre complet d'un de vos dossiers faits à la
main). Un relais peut en revanche présenter d'autres relais. L'application Android rejoint un réseau, mais ne crée
pas d'invitations d'admin.

**À qui faire confiance.** Vous ne faites confiance qu'aux admins que **vous** avez choisis : celui dont vous avez
scanné le code (il est affiché avant que vous confirmiez), et ceux qu'un admin de confiance a promus (sans limite
de profondeur : promouvoir est un acte aussi grave que d'être admin). Un admin compromis peut introduire de
nouveaux appareils dans les dossiers qu'il partage avec vous, et ces appareils recevront alors le secret et les
fichiers : traitez un admin comme un appareil complet de confiance. Pour retirer un admin, supprimez-le
(`device remove`) sur chacun de vos appareils.

**Confirmer une demande.** Le nouvel appareil affiche son propre ID au moment de se connecter ; l'admin voit le même
dans la demande. Comparez-les : un code QR vu par quelqu'un d'autre permettrait sinon à cette personne de faire une
demande sous un nom plausible.

## Ce qui est écrit où

- `config.json` : `admin` (cet appareil est admin), `devices[].admin` (admin de confiance, choisi par vous),
  `devices[].managed` et `folders[].managed` (entrées ajoutées par une autorisation, donc retirables par une
  autre), `network_dir` (où créer les dossiers reçus), `dismissed` (ce que vous avez retiré à la main).
  **Ce que vous avez créé à la main n'est jamais modifié ni supprimé par une autorisation.**
- `devices[].full` : vous avez reconnu cet appareil comme détenteur de copies complètes (posé quand vous rejoignez via
  une invitation « copie complète »).
- `grants.json` : les autorisations signées reçues ou émises, une par admin et par appareil. Perdu ou corrompu, il est reconstruit par les autres
  appareils sans rien enlever à la configuration.
- Un dossier reçu d'un admin attend son secret (`awaiting_secret`) tant qu'aucun appareil complet connecté ne
  le lui a remis ; il démarre ensuite tout seul.

## Limites connues

- Un changement d'appartenance redémarre le démon des appareils concernés (comme tout changement de
  configuration) ; les connexions reviennent en quelques secondes.
- Un appareil apprend un nouveau venu quand il se **reconnecte** à un appareil qui le sait : si l'admin est
  éteint, les autres l'apprennent dès qu'il revient (ou par n'importe quel appareil qui le sait déjà).
- Si un appareil est retiré à la main (`device remove`) alors qu'une autorisation le lui accorde encore, il ne
  revient qu'avec une autorisation plus récente. Même chose pour un dossier reçu et retiré sur le téléphone.
- Les horloges des admins servent à ordonner les autorisations ; une date plus de 24 h dans le futur est refusée.
- Un appareil apprend le nom et les dossiers des appareils qui partagent un dossier avec lui (pas les autres) ;
  il apprend aussi les identités des admins. Les adresses (« où le joindre ») ne sont retenues que si elles viennent
  d'un admin de confiance, et seulement les adresses IP, jamais des noms de machine.
- Une autorisation qui ne peut pas être appliquée (par exemple elle rendrait un de vos dossiers invalide) est
  ignorée dans le journal sans bloquer les autres.
