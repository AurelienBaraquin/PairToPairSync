# Présentation automatique : se connecter à un seul appareil

Sans cela, chaque appareil devrait être appairé avec chacun des autres (A↔B, A↔C, B↔C…). Avec la présentation
automatique, un nouvel appareil n'a qu'**un seul** appairage à faire, avec n'importe quel appareil de votre réseau :
celui-ci le **présente** aux autres appareils des mêmes dossiers, et les leur présente. Il n'y a ni rôle spécial, ni
compte, ni serveur : **tout appareil qui détient une copie complète présente ceux qu'il a appairés.**

## En pratique

Sur un appareil du réseau (le démon tourne) :

```sh
p2psync invite                                  # tous vos dossiers en copie complète
p2psync invite --folders docs,photos            # seulement ceux-là
p2psync invite --as-relay                       # le nouvel appareil sera un relais (téléphone)
p2psync invite --confirm                        # je veux accepter la demande à la main (voir plus bas)
```

Sur le nouvel appareil :

```sh
p2psync pair <texte du QR> --dir D:\P2PSync     # Windows ;  --dir ~/P2PSync sous Ubuntu
```

(L'application Android scanne le QR directement.) La commande affiche qui invite, quels dossiers sont proposés et où
ils seront créés, puis demande confirmation. Le nouvel appareil crée chaque dossier sous `--dir`, reçoit leurs
secrets (copie complète) et apprend l'existence des autres appareils de ces dossiers ainsi que **où les joindre**. Les
autres appareils l'apprennent de l'appareil qui l'a appairé, la prochaine fois qu'ils s'y connectent, et le nouvel
appareil et eux se connectent **directement**, même si celui par lequel il est entré est éteint.

Exemple : votre réseau est un PC (A), un portable (B) et un téléphone-relais (R1). Vous appairez un second téléphone
R2 avec A seul. R2 apprend B (le portable) ; B apprend R2 ; ils se connectent. R2 n'est **pas** présenté à R1 : deux
relais ne se parlent jamais, ils n'en ont pas besoin.

Avec `--confirm`, la demande reste en attente chez l'appareil qui invite jusqu'à ce que vous répondiez :

```sh
p2psync requests          # nom, ID complet et adresse de l'appareil qui demande
p2psync accept <demande>  # ou: p2psync reject <demande>    (ou boutons sur la page locale : p2psync ui)
```

Le nouvel appareil affiche son propre ID au moment de se connecter : comparez-le à celui de la demande (un QR vu
par quelqu'un d'autre permettrait sinon de faire une demande sous un nom plausible). C'est utile si le QR a pu être
vu ; sinon l'appairage est immédiat, comme d'habitude.

Autres commandes : `p2psync network` (qui est qui) ; `p2psync device remove <ID>` retire l'appareil de votre
configuration **et annule ce que vous aviez dit de lui** aux autres.

## Qui peut présenter qui

Une présentation est un message **signé** avec la clé de l'appareil qui la fait (« l'appareil S participe aux dossiers
F, en copie complète ou en relais »). Chaque appareil le vérifie lui-même.

| Règle | Pourquoi |
|---|---|
| **Seuls les appareils à copie complète présentent.** Un relais (téléphone) ne présente personne | un relais n'a jamais le secret des dossiers ; s'il pouvait faire entrer un appareil complet, celui-ci recevrait le secret d'un autre appareil et le relais ne serait plus aveugle |
| On ne présente que **aux dossiers dont on est soi-même membre**, et seulement les appareils de ces dossiers | on ne peut pas donner accès à ce qu'on n'a pas ; un appareil n'apprend pas l'existence d'appareils avec lesquels il ne partage rien |
| On fait confiance aux appareils complets **appairés par vous**, et à ceux qu'un appareil de confiance a présentés comme complets | la confiance suit les appairages que vous avez réellement faits, pas ce que raconte un appareil inconnu |
| Un appareil ne peut pas se porter garant de lui-même, ni se dire « copie complète » | sinon un relais pourrait se faire passer pour un appareil complet |
| Un nouveau dossier en copie complète n'est créé que sur la parole d'un appareil que **vous** avez reconnu comme détenteur de copies complètes (vous avez scanné son code pour une copie complète, ou il est membre complet d'un de vos dossiers faits à la main) | le secret du dossier sera demandé à cet appareil |
| Ce qu'un appareil reçoit est la **réunion** de ce que disent les appareils de confiance | l'un qui restreint ou retire sa présentation n'efface pas celle d'un autre |

**Ce que cela suppose.** Vos appareils complets se font confiance entre eux, en chaîne : chacun détient déjà tous les
fichiers et le secret, donc l'un d'eux compromis est déjà un problème, mais il peut en plus **faire entrer de
nouveaux appareils** dans les dossiers qu'il partage avec vous, et ceux-ci recevront alors le secret et les fichiers.
Si un appareil complet est perdu ou volé : retirez-le (`device remove`) sur **chacun** de vos appareils qui l'ont
appairé directement. Un appareil qui n'a plus d'appairage direct nulle part n'est plus présenté à personne.

## Ce qui est écrit où

- `config.json` : `devices[].managed` et `folders[].managed` (entrées ajoutées par une présentation, donc retirables
  par une autre ; **ce que vous avez créé ou appairé n'est jamais modifié ni supprimé par une présentation**),
  `devices[].full` (vous avez reconnu cet appareil comme détenteur de copies complètes), `network_dir` (où créer les
  dossiers reçus), `dismissed` (ce que vous avez retiré à la main : ne revient qu'avec une présentation plus récente).
- `grants.json` : les présentations signées reçues ou émises, une par appareil qui présente et par appareil présenté.
  Perdu ou corrompu, il est reconstruit par les autres appareils sans rien enlever à la configuration.
- Un dossier reçu attend son secret (`awaiting_secret`) tant qu'aucun appareil complet connecté ne le lui a remis ; il
  démarre ensuite tout seul.

## Plusieurs relais

Les relais ne se parlent pas et chacun travaille seul. Un appareil complet qui a un changement l'annonce à tous les
relais connectés ; chacun attend la confirmation de **tous les appareils complets** avant d'effacer, jamais celle d'un
autre relais. Un appareil qui revient télécharge chaque modification une seule fois (depuis la première source
disponible) et confirme à tous. Un relais qui disparaît ne bloque pas l'autre ; ce qu'il détenait expire avec la durée
de rétention (30 jours). Coût : quand un appareil est absent, chaque relais va chercher les blocs chez l'expéditeur, qui
envoie donc une fois par relais, et chaque téléphone stocke sa propre copie chiffrée.

## Limites connues

- Un changement d'appartenance redémarre le démon des appareils concernés (comme tout changement de configuration) ;
  les connexions reviennent en quelques secondes.
- Un appareil apprend un nouveau venu quand il se **reconnecte** à un appareil qui le sait.
- Retirer un appareil sur un seul de vos appareils ne suffit pas si un autre l'a appairé directement : il reste présenté
  par celui-là.
- Les appareils présentés à un nouveau venu sont ceux des dossiers partagés (et leurs noms) ; les adresses ne sont retenues
  que si elles viennent d'un appareil de confiance, et seulement les adresses IP, jamais des noms de machine.
- Une présentation qui ne peut pas être appliquée (par exemple elle rendrait un de vos dossiers invalide) est ignorée dans le
  journal sans bloquer les autres. Une date plus de 24 h dans le futur est refusée.
- Un téléphone n'a pas d'interface pour créer des invitations d'appareils complets : il ne détient pas de copie.
