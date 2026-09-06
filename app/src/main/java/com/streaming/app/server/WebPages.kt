package com.streaming.app.server

object WebPages {

    fun controlPage(): String = """
        <!DOCTYPE html>
        <html lang="en">
        <head>
          <meta charset="utf-8">
          <meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
          <title>Streaming</title>
          <style>
            :root {
              color-scheme: dark;
              --bg: #000000;
              --surface: #111111;
              --text: #e8eaed;
              --muted: #9aa0a6;
              --accent: #8ab4f8;
              --ring: #5f6368;
              --active: #ffffff;
            }
            * { box-sizing: border-box; }
            body {
              margin: 0;
              min-height: 100vh;
              background: var(--bg);
              color: var(--text);
              font-family: "Google Sans", "Roboto", "Segoe UI", sans-serif;
              display: flex;
              align-items: center;
              justify-content: center;
              padding: 24px;
            }
            .panel {
              width: min(520px, 100%);
            }
            .status {
              min-height: 72px;
              margin-bottom: 32px;
              padding: 20px 24px;
              border-radius: 28px;
              background: var(--surface);
              display: flex;
              flex-direction: column;
              justify-content: center;
              gap: 8px;
            }
            .status-label {
              font-size: 14px;
              letter-spacing: 0.08em;
              text-transform: uppercase;
              color: var(--muted);
            }
            .status-value {
              font-size: 28px;
              line-height: 1.2;
            }
            .status-error {
              font-size: 14px;
              color: #f28b82;
              min-height: 18px;
            }
            .status.unreachable {
              border: 1px solid #f28b82;
            }
            .choices {
              display: grid;
              gap: 16px;
            }
            .choice {
              border: 1px solid var(--ring);
              background: transparent;
              color: var(--text);
              border-radius: 999px;
              min-height: 72px;
              font-size: 22px;
              cursor: pointer;
              transition: background 120ms ease, border-color 120ms ease, color 120ms ease;
            }
            .choice.active {
              background: var(--active);
              color: #000000;
              border-color: var(--active);
            }
            .choice:disabled,
            .stop:disabled {
              opacity: 0.35;
              cursor: not-allowed;
            }
            .stop {
              margin-top: 24px;
              width: 100%;
              min-height: 64px;
              border-radius: 999px;
              border: 1px solid #5f3634;
              background: #1f1413;
              color: #f28b82;
              font-size: 20px;
              cursor: pointer;
            }
            .footer {
              margin-top: 28px;
              text-align: center;
            }
            .footer a {
              color: var(--accent);
              text-decoration: none;
              font-size: 15px;
            }
          </style>
        </head>
        <body>
          <main class="panel">
            <section class="status" aria-live="polite">
              <div class="status-label">Status</div>
              <div id="statusValue" class="status-value">Loading...</div>
              <div id="statusError" class="status-error"></div>
            </section>

            <div class="choices" role="radiogroup" aria-label="Streaming language">
              <button id="englishButton" class="choice" type="button" data-stream="english">English</button>
              <button id="mandarinButton" class="choice" type="button" data-stream="mandarin">Mandarin</button>
            </div>

            <button id="stopButton" class="stop" type="button" disabled>Stop</button>

            <div class="footer">
              <a href="/config">Configuration</a>
            </div>
          </main>

          <script>
            const statusValue = document.getElementById("statusValue");
            const statusError = document.getElementById("statusError");
            const statusPanel = document.querySelector(".status");
            const englishButton = document.getElementById("englishButton");
            const mandarinButton = document.getElementById("mandarinButton");
            const stopButton = document.getElementById("stopButton");

            let busy = false;

            function setActive(stream) {
              englishButton.classList.toggle("active", stream === "english");
              mandarinButton.classList.toggle("active", stream === "mandarin");
            }

            function applyState(data) {
              const reachable = Boolean(data.smpReachable);
              statusValue.textContent = data.statusMessage || "Idle";
              statusError.textContent = data.lastError || "";
              statusPanel.classList.toggle("unreachable", !reachable);
              setActive(data.activeStream);
              englishButton.disabled = busy || !reachable;
              mandarinButton.disabled = busy || !reachable;
              stopButton.disabled = busy || !reachable || !data.streamEnabled;
            }

            async function refreshStatus() {
              try {
                const response = await fetch("/api/status", { cache: "no-store" });
                const data = await response.json();
                applyState(data);
              } catch (error) {
                statusValue.textContent = "Server unavailable";
                statusError.textContent = String(error);
                statusPanel.classList.add("unreachable");
                englishButton.disabled = true;
                mandarinButton.disabled = true;
                stopButton.disabled = true;
              }
            }

            async function postAction(path) {
              if (busy) return;
              busy = true;
              englishButton.disabled = true;
              mandarinButton.disabled = true;
              stopButton.disabled = true;
              try {
                const response = await fetch(path, { method: "POST" });
                const data = await response.json();
                applyState(data);
              } catch (error) {
                statusValue.textContent = "SMP is not reachable";
                statusError.textContent = String(error);
                statusPanel.classList.add("unreachable");
              } finally {
                busy = false;
                await refreshStatus();
              }
            }

            englishButton.addEventListener("click", () => postAction("/api/stream/english"));
            mandarinButton.addEventListener("click", () => postAction("/api/stream/mandarin"));
            stopButton.addEventListener("click", () => postAction("/api/stream/stop"));

            refreshStatus();
            setInterval(refreshStatus, 3000);
          </script>
        </body>
        </html>
    """.trimIndent()

    fun configPage(): String = """
        <!DOCTYPE html>
        <html lang="en">
        <head>
          <meta charset="utf-8">
          <meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
          <title>Streaming Configuration</title>
          <style>
            :root {
              color-scheme: dark;
              --bg: #000000;
              --surface: #111111;
              --text: #e8eaed;
              --muted: #9aa0a6;
              --accent: #8ab4f8;
              --ring: #3c4043;
            }
            * { box-sizing: border-box; }
            body {
              margin: 0;
              min-height: 100vh;
              background: var(--bg);
              color: var(--text);
              font-family: "Google Sans", "Roboto", "Segoe UI", sans-serif;
              padding: 24px;
            }
            .panel {
              width: min(640px, 100%);
              margin: 0 auto;
            }
            h1 {
              font-size: 28px;
              margin: 0 0 8px;
            }
            .hint {
              color: var(--muted);
              margin-bottom: 28px;
              line-height: 1.5;
            }
            form {
              display: grid;
              gap: 18px;
            }
            label {
              display: grid;
              gap: 8px;
              font-size: 14px;
              color: var(--muted);
            }
            input {
              width: 100%;
              min-height: 52px;
              border-radius: 16px;
              border: 1px solid var(--ring);
              background: var(--surface);
              color: var(--text);
              padding: 0 16px;
              font-size: 16px;
            }
            .actions {
              display: flex;
              gap: 12px;
              margin-top: 8px;
            }
            button, .link-button {
              min-height: 52px;
              border-radius: 999px;
              border: none;
              font-size: 16px;
              cursor: pointer;
              padding: 0 20px;
            }
            button[type="submit"] {
              background: var(--text);
              color: #000000;
              flex: 1;
            }
            .link-button {
              display: inline-flex;
              align-items: center;
              justify-content: center;
              background: transparent;
              color: var(--accent);
              text-decoration: none;
              border: 1px solid var(--ring);
            }
            #saveMessage {
              min-height: 20px;
              color: var(--accent);
            }
            .section-title {
              margin-top: 12px;
              font-size: 13px;
              letter-spacing: 0.08em;
              text-transform: uppercase;
              color: var(--muted);
            }
          </style>
        </head>
        <body>
          <main class="panel">
            <h1>Configuration</h1>
            <p class="hint">
              Set the Extron SMP connection once. Credentials stay on this tablet only.
              Changing the HTTP port restarts the local server.
            </p>

            <form id="configForm">
              <div class="section-title">Extron SMP</div>
              <label>
                SMP hostname or IP
                <input id="smpHost" name="smpHost" type="text" autocomplete="off" required>
              </label>
              <label>
                SMP SSH port
                <input id="smpSshPort" name="smpSshPort" type="number" min="1" max="65535" required>
              </label>
              <label>
                SSH username
                <input id="smpUsername" name="smpUsername" type="text" autocomplete="username" required>
              </label>
              <label>
                SSH password
                <input id="smpPassword" name="smpPassword" type="password" autocomplete="current-password">
              </label>

              <div class="section-title">Local server</div>
              <label>
                HTTP port for Fully Kiosk Browser
                <input id="httpPort" name="httpPort" type="number" min="1024" max="65535" required>
              </label>

              <div class="section-title">Streaming presets</div>
              <label>
                Stream index
                <input id="streamIndex" name="streamIndex" type="number" min="1" max="3" required>
              </label>
              <label>
                English preset number
                <input id="englishPreset" name="englishPreset" type="number" min="1" max="32" required>
              </label>
              <label>
                Mandarin preset number
                <input id="mandarinPreset" name="mandarinPreset" type="number" min="1" max="32" required>
              </label>

              <div id="saveMessage"></div>

              <div class="actions">
                <a class="link-button" href="/">Back</a>
                <button type="submit">Save</button>
              </div>
            </form>
          </main>

          <script>
            const form = document.getElementById("configForm");
            const saveMessage = document.getElementById("saveMessage");

            async function loadConfig() {
              const response = await fetch("/api/config");
              const data = await response.json();
              document.getElementById("smpHost").value = data.smpHost || "";
              document.getElementById("smpSshPort").value = data.smpSshPort || 22023;
              document.getElementById("smpUsername").value = data.smpUsername || "";
              document.getElementById("httpPort").value = data.httpPort || 8080;
              document.getElementById("streamIndex").value = data.streamIndex || 1;
              document.getElementById("englishPreset").value = data.englishPreset || 2;
              document.getElementById("mandarinPreset").value = data.mandarinPreset || 1;
            }

            form.addEventListener("submit", async (event) => {
              event.preventDefault();
              saveMessage.textContent = "Saving...";
              const payload = {
                smpHost: document.getElementById("smpHost").value.trim(),
                smpSshPort: Number(document.getElementById("smpSshPort").value),
                smpUsername: document.getElementById("smpUsername").value.trim(),
                smpPassword: document.getElementById("smpPassword").value,
                httpPort: Number(document.getElementById("httpPort").value),
                streamIndex: Number(document.getElementById("streamIndex").value),
                englishPreset: Number(document.getElementById("englishPreset").value),
                mandarinPreset: Number(document.getElementById("mandarinPreset").value)
              };

              const response = await fetch("/api/config", {
                method: "POST",
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify(payload)
              });
              const data = await response.json();
              if (response.ok) {
                saveMessage.textContent = "Saved. Server restarted on port " + data.httpPort + ".";
                document.getElementById("smpPassword").value = "";
              } else {
                saveMessage.textContent = data.error || "Save failed";
              }
            });

            loadConfig().catch((error) => {
              saveMessage.textContent = String(error);
            });
          </script>
        </body>
        </html>
    """.trimIndent()
}
