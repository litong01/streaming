# Streaming

Android app for tablet-based control of Extron SMP 300 Series streaming presets. A foreground service hosts a local web UI for Fully Kiosk Browser. English and Mandarin map to SMP streaming presets 1 and 2 by default.

## What it does

- Runs a background HTTP server on the tablet (default port `8080`)
- Serves a one-page control UI with status, English, Mandarin, and Stop
- Serves a configuration page for SMP SSH settings and local HTTP port
- Sends Extron SIS commands over SSH to recall presets and start or stop streaming
- Polls the SMP for live status
- Stores credentials on the device only (encrypted preferences)
- Starts again after reboot

## Typical setup

1. Install the APK on the tablet that runs Fully Kiosk Browser.
2. Open the Streaming app once and start the server.
3. Open `http://127.0.0.1:8080/config` and enter:
   - SMP hostname or IP
   - SMP SSH port (default `22023`)
   - SSH username and password
   - HTTP port for Fully Kiosk (default `8080`)
4. Point Fully Kiosk Browser at `http://127.0.0.1:8080/` (same tablet) or `http://<tablet-ip>:8080/` (another device on the LAN).
5. Use the control page day to day. Reopen the Android app only when settings change.

Presets 1 (English) and 2 (Mandarin) must already exist on the SMP via the Extron web interface.

## SMP commands used

For stream index `N` and preset `P`:

- Recall streaming preset: `3*N*P.`
- Enable stream: `E N*1 STRC}`
- Disable stream: `E N*0 STRC}`
- Query stream enabled: `E N)STRC}`
- Query active streaming preset: `3*N)STRP}`

Default stream index is `1` (Archive Ch A).

## Build

Requirements:

- Android SDK (API 34)
- JDK 17

Create `local.properties` with your SDK path:

```properties
sdk.dir=/path/to/Android/sdk
```

Build debug APK:

```bash
./gradlew assembleDebug
```

Output: `app/build/outputs/apk/debug/app-debug.apk`

Build release AAB (after configuring signing):

```bash
./gradlew bundleRelease
```

## Project layout

- `app/src/main/java/com/streaming/app/service/` foreground service and server lifecycle
- `app/src/main/java/com/streaming/app/server/` NanoHTTPD routes and web pages
- `app/src/main/java/com/streaming/app/smp/` SSH client and SIS command helpers
- `app/src/main/java/com/streaming/app/config/` encrypted on-device configuration

## Notes

- HTTP is plain text so Fully Kiosk can use `http://` URLs on the LAN or localhost.
- English and Mandarin are exclusive: starting one recalls its preset and enables streaming.
- Stop disables streaming on the configured stream index. If nothing is streaming, Stop is disabled in the UI.
- Future work: custom preset (for example preset 11) as a third streaming option.
