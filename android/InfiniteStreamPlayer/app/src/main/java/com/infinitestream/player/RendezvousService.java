package com.infinitestream.player;

import android.content.Context;
import android.content.SharedPreferences;
import android.os.Handler;
import android.os.Looper;

import org.json.JSONArray;
import org.json.JSONObject;

import java.io.BufferedReader;
import java.io.InputStreamReader;
import java.net.HttpURLConnection;
import java.net.URL;
import java.security.SecureRandom;
import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.atomic.AtomicBoolean;

/**
 * Talks to the InfiniteStream pairing rendezvous Worker so the user can
 * pick a server visible from their public IP instead of typing a URL.
 *
 * The standalone /pair page on the Worker also uses this same data; here
 * we just consume GET /announce to populate a chooser dialog.
 */
public final class RendezvousService {

    /** Default Worker URL baked into the build. Override in SharedPreferences
     *  ("InfiniteStreamRendezvousURL") if you self-host the Worker. Empty
     *  disables discovery. */
    public static final String DEFAULT_URL =
        "https://pair-infinitestream.jeoliver.com";

    private static final String PREFS = "rendezvous";
    private static final String PREF_URL = "InfiniteStreamRendezvousURL";

    private static final ExecutorService EXEC = Executors.newSingleThreadExecutor();
    private static final Handler MAIN = new Handler(Looper.getMainLooper());

    private RendezvousService() {}

    /** Effective Worker URL (override or default). */
    public static String url(Context ctx) {
        SharedPreferences prefs = ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE);
        String override = prefs.getString(PREF_URL, "").trim();
        return override.isEmpty() ? DEFAULT_URL : override;
    }

    /** One detected server reported by GET /announce. */
    public static final class DiscoveredServer {
        public final String serverId;
        public final String url;
        public final String label;
        public DiscoveredServer(String serverId, String url, String label) {
            this.serverId = serverId;
            this.url = url;
            this.label = label;
        }
    }

    public interface DiscoverCallback {
        /** Called on the main thread. error is null on success. */
        void onResult(List<DiscoveredServer> servers, String error);
    }

    /** Async list of servers visible from the caller's public IP. */
    public static void discoverServers(Context ctx, DiscoverCallback cb) {
        final String base = url(ctx);
        if (base.isEmpty()) {
            MAIN.post(() -> cb.onResult(new ArrayList<>(), "Rendezvous URL not configured"));
            return;
        }
        final String endpoint = base.replaceAll("/+$", "") + "/announce";
        EXEC.execute(() -> {
            List<DiscoveredServer> out = new ArrayList<>();
            String error = null;
            HttpURLConnection conn = null;
            try {
                URL u = new URL(endpoint);
                conn = (HttpURLConnection) u.openConnection();
                conn.setRequestMethod("GET");
                conn.setConnectTimeout(8000);
                conn.setReadTimeout(8000);
                conn.setRequestProperty("Cache-Control", "no-cache");
                int code = conn.getResponseCode();
                if (code != 200) {
                    error = "HTTP " + code;
                } else {
                    StringBuilder sb = new StringBuilder();
                    try (BufferedReader r = new BufferedReader(new InputStreamReader(conn.getInputStream()))) {
                        String line;
                        while ((line = r.readLine()) != null) sb.append(line);
                    }
                    JSONObject obj = new JSONObject(sb.toString());
                    JSONArray arr = obj.optJSONArray("servers");
                    if (arr != null) {
                        for (int i = 0; i < arr.length(); i++) {
                            JSONObject s = arr.getJSONObject(i);
                            String id = s.optString("server_id", "");
                            String url = s.optString("url", "");
                            String label = s.optString("label", "");
                            if (id.isEmpty() || url.isEmpty()) continue;
                            out.add(new DiscoveredServer(id, url, label.isEmpty() ? url : label));
                        }
                    }
                }
            } catch (Exception e) {
                error = e.getClass().getSimpleName() + ": " + e.getMessage();
            } finally {
                if (conn != null) conn.disconnect();
            }
            final List<DiscoveredServer> servers = out;
            final String err = error;
            MAIN.post(() -> cb.onResult(servers, err));
        });
    }

    /** Generates a 6-character pairing code from an ambiguity-free alphabet
     *  (no 0/O, 1/I/L). Matches the iOS/tvOS code style. */
    public static String generateCode() {
        final char[] alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789".toCharArray();
        SecureRandom rng = new SecureRandom();
        StringBuilder sb = new StringBuilder(6);
        for (int i = 0; i < 6; i++) sb.append(alphabet[rng.nextInt(alphabet.length)]);
        return sb.toString();
    }

    public interface PollCallback {
        /** Called on the main thread. serverURL is non-null on success. */
        void onResult(String serverURL, String error);
    }

    /** Returns a Runnable handle that, when invoked, cancels the poll and
     *  fires a best-effort DELETE on the entry. */
    public interface Cancel { void cancel(); }

    /** Polls the rendezvous /pair endpoint every pollIntervalMs until a
     *  server URL appears or timeoutMs elapses. cb is called once on the
     *  main thread. Cancel.cancel() stops polling and DELETEs the entry. */
    public static Cancel pollForServerURL(Context ctx, String code,
                                          long pollIntervalMs, long timeoutMs,
                                          PollCallback cb) {
        final String base = url(ctx);
        if (base.isEmpty()) {
            MAIN.post(() -> cb.onResult(null, "Rendezvous URL not configured"));
            return () -> {};
        }
        final String endpoint = base.replaceAll("/+$", "") + "/pair?code=" + code;
        final AtomicBoolean cancelled = new AtomicBoolean(false);
        EXEC.execute(() -> {
            long deadline = System.currentTimeMillis() + timeoutMs;
            String result = null;
            String error = null;
            while (!cancelled.get() && System.currentTimeMillis() < deadline) {
                HttpURLConnection conn = null;
                try {
                    URL u = new URL(endpoint);
                    conn = (HttpURLConnection) u.openConnection();
                    conn.setRequestMethod("GET");
                    conn.setConnectTimeout(8000);
                    conn.setReadTimeout(8000);
                    conn.setRequestProperty("Cache-Control", "no-cache");
                    int code2 = conn.getResponseCode();
                    if (code2 == 200) {
                        StringBuilder sb = new StringBuilder();
                        try (BufferedReader r = new BufferedReader(new InputStreamReader(conn.getInputStream()))) {
                            String line;
                            while ((line = r.readLine()) != null) sb.append(line);
                        }
                        String body = sb.toString().trim();
                        if (!body.isEmpty()) { result = body; break; }
                    } else if (code2 == 204) {
                        // keep polling
                    } else if (code2 == 403) {
                        error = "Cross-network pairing blocked: the publisher's public IP differs from this TV's.";
                        break;
                    } else {
                        error = "HTTP " + code2;
                        break;
                    }
                } catch (Exception e) {
                    // transient network blip — keep polling
                } finally {
                    if (conn != null) conn.disconnect();
                }
                try { Thread.sleep(pollIntervalMs); } catch (InterruptedException ignored) {}
            }
            if (result == null && error == null && !cancelled.get()) {
                error = "Pairing timed out — code was not entered in time.";
            }
            final String r = result;
            final String e = error;
            if (!cancelled.get()) MAIN.post(() -> cb.onResult(r, e));
        });
        return () -> {
            cancelled.set(true);
            releaseCode(ctx, code);
        };
    }

    /** Best-effort DELETE of a code; failure is ignored. */
    public static void releaseCode(Context ctx, String code) {
        final String base = url(ctx);
        if (base.isEmpty()) return;
        final String endpoint = base.replaceAll("/+$", "") + "/pair?code=" + code;
        EXEC.execute(() -> {
            HttpURLConnection conn = null;
            try {
                URL u = new URL(endpoint);
                conn = (HttpURLConnection) u.openConnection();
                conn.setRequestMethod("DELETE");
                conn.setConnectTimeout(5000);
                conn.setReadTimeout(5000);
                conn.getResponseCode();
            } catch (Exception ignored) {
            } finally {
                if (conn != null) conn.disconnect();
            }
        });
    }
}
