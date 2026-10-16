# Application Android — téléphone en relais temporaire

L'application transforme un téléphone en **point de relais** : elle garde les dernières modifications de vos
ordinateurs jusqu'à ce que chacun les ait reçues, puis les oublie. Elle n'est jamais une copie du dossier et
**ne peut rien lire** de ce qu'elle porte (tout est chiffré pour vos ordinateurs seulement).

Interface Kotlin / Jetpack Compose ; le moteur est le même code Go que sur ordinateur, compilé en bibliothèque
Android avec `gomobile` (paquet [`../mobile`](../mobile)).

## Utilisation

1. Sur l'ordinateur : `p2psync invite --as-relay` affiche un QR code.
2. Sur le téléphone : **Scanner un QR code** (ou ouvrir le lien `p2psync://pair/…` depuis n'importe quelle
   appli de lecture de QR, ou **Coller un code**). L'application affiche avec qui et pour quel dossier, et
   **demande confirmation** : un lien ne peut jamais appairer le téléphone sans votre accord.
3. Activer **Relais actif**. Un service au premier plan maintient la connexion (notification permanente).
4. Pour qu'un autre ordinateur utilise aussi ce relais : **Afficher un QR code pour un ordinateur**, puis
   `p2psync pair <code>` sur cet ordinateur.

Réglages : *Wi‑Fi uniquement* (activé par défaut : pause sur données mobiles), découverte sur le réseau local,
nom de l'appareil, limite de stockage et durée de rétention par dossier, appareils appairés, diagnostic
(journal gardé **en mémoire seulement**).

## Ce que fait le service

- Redémarre le moteur à chaque changement de réseau (Wi‑Fi ↔ données mobiles, nouveau point d'accès).
- En mode *Wi‑Fi uniquement*, arrête le moteur hors réseau non facturé et le relance au retour du Wi‑Fi.
- Détient le *multicast lock* Wi‑Fi nécessaire à la découverte mDNS.
- Redémarre après un redémarrage du téléphone si vous l'aviez activé.
- Vous invite à exempter l'application des optimisations de batterie (sans permission spéciale : ouvre
  simplement le réglage système).

## Sécurité côté Android

- `allowBackup=false` et règles d'extraction vides : la **clé privée de l'appareil ne quitte jamais le
  téléphone** (ni sauvegarde cloud, ni transfert).
- Données dans le stockage privé de l'application ; pas de trafic en clair ; aucun journal système.
- Permissions : réseau, premier plan, notifications, démarrage, multicast Wi‑Fi, et l'appareil photo
  **uniquement** pour lire un QR code (demandée à l'ouverture du scanner).
- Le QR code d'invitation affiché est un identifiant : écran protégé (`FLAG_SECURE`, pas de capture), copie
  marquée « sensible » dans le presse-papiers.
- Lecture de QR avec ZXing (open source, aucun service Google).

## Compiler

Prérequis : JDK 17+, Go, Android SDK (plateforme 36) + NDK 27, `gomobile`.

```sh
export ANDROID_HOME=~/Android/Sdk
go install golang.org/x/mobile/cmd/gomobile golang.org/x/mobile/cmd/gobind   # version épinglée par go.mod
./android/scripts/build-aar.sh          # moteur Go -> android/app/libs/p2psync.aar (arm64)
cd android && ./gradlew assembleDebug   # -> app/build/outputs/apk/debug/app-debug.apk
./gradlew testDebugUnitTest lintDebug
```

Seule l'architecture arm64 est livrée (les téléphones actuels) ; l'émulateur x86 n'est donc pas pris en charge.
La bibliothèque native est alignée sur des pages de 16 Ko (Android 15+) et l'APK est vérifié avec `zipalign -P 16`.
Signature de release : définissez `P2PSYNC_KEYSTORE`, `P2PSYNC_KEYSTORE_PASSWORD`, `P2PSYNC_KEY_ALIAS` (et
éventuellement `P2PSYNC_KEY_PASSWORD`) avant `./gradlew assembleRelease` ; sans eux l'APK reste non signé. Aucune clé
n'est fournie.

## Ce qui est vérifié, ce qui ne l'est pas

Vérifié : le moteur Go via l'API `mobile` (tests Linux avec `-race` : appairage, démarrage supervisé, invitation
depuis le téléphone, vues de configuration sans secret) ; la compilation de l'AAR et de l'APK ; le manifeste
fusionné ; les tests JVM du code Kotlin sur de **vraies sorties du moteur** ; le lint (0 erreur).

**Non vérifié : l'exécution sur un téléphone ou un émulateur** (aucun n'était disponible). Restent à valider sur
appareil : le scanner caméra, le service au premier plan et ses restrictions selon les constructeurs, la
découverte mDNS, le comportement sur batterie et la reprise après redémarrage.

## Limites

- Pas de verrou Wi‑Fi (`WifiLock`) : quand l'écran est éteint, certains téléphones coupent la radio Wi‑Fi et le relais
  n'est alors joignable que par intermittence. À mesurer sur appareil avant d'ajouter ce verrou (coût en batterie).

- Le téléphone est **relais seulement** : une copie complète demanderait l'accès à tout le stockage.
- Les interfaces sont en anglais et en français ; le reste du monde reçoit l'anglais.
- Distribution : APK à installer soi-même (non publié sur un magasin). Le type de service `specialUse` demande
  une justification lors d'une publication sur le Play Store.
