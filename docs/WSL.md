# Ubuntu et WSL

Le binaire Linux (`make linux` → `dist/p2psync-linux-amd64`, statique) fonctionne sur Ubuntu et dans WSL2.
Installation : copier le binaire dans `~/.local/bin/p2psync` (`chmod +x`).

```sh
p2psync init --name ubuntu
p2psync folder add ~/Projets --id projets
p2psync run &                         # ou le service systemd ci-dessous
```

## Lancer au démarrage

Ubuntu : `packaging/p2psync.service` (unité *utilisateur* systemd) — instructions en tête du fichier. WSL2 :
activez d'abord systemd (`/etc/wsl.conf` : `[boot]` puis `systemd=true`, puis `wsl --shutdown` côté Windows), ou
lancez `wsl.exe -d Ubuntu -- p2psync run` depuis une tâche planifiée Windows.

## Ce qu'il faut savoir sur WSL

- **Gardez les dossiers dans le système de fichiers Linux** (`~/…`). Les disques Windows vus de WSL (`/mnt/c/…`)
  ne notifient pas les modifications faites par des programmes Windows : p2psync le détecte, le dit dans le
  journal et rescanne alors toutes les 10 s au lieu d'être instantané. Les noms y suivent aussi les règles de
  Windows (insensibles à la casse).
- **Réseau.** Par défaut WSL2 est derrière un NAT virtuel : les autres appareils du réseau local ne peuvent pas
  s'y connecter et la découverte locale (mDNS) n'en sort pas. Deux solutions : le **mode réseau miroir**
  (Windows 11 22H2+, fichier `%UserProfile%\.wslconfig` : `[wsl2]` puis `networkingMode=mirrored`, puis
  `wsl --shutdown`), ou laisser l'instance WSL se connecter vers l'extérieur (les connexions sortantes marchent) et
  faire passer le reste par un autre de vos appareils joignable, qui sert de relais de connexion. En l'état, WSL
  est un bon client mais un mauvais appareil « joignable ».
- Un PC Windows **et** son WSL sont deux appareils distincts : chacun a son identité et sa clé.
- Rien de ceci n'a été exécuté dans un vrai WSL par l'auteur : la détection des disques Windows et l'unité systemd
  sont testées sous Linux ordinaire seulement.
