package com.infinitestream.player;

import android.content.Context;
import android.content.SharedPreferences;
import android.content.res.ColorStateList;
import android.net.Uri;
import android.os.Bundle;
import android.os.Handler;
import android.os.Looper;
import android.view.Gravity;
import android.view.View;
import android.view.ViewGroup;
import android.view.WindowManager;
import android.widget.EditText;
import android.widget.LinearLayout;
import android.widget.TextView;
import android.widget.Toast;

import androidx.constraintlayout.widget.ConstraintLayout;

import androidx.annotation.OptIn;
import androidx.appcompat.app.AlertDialog;
import androidx.appcompat.app.AppCompatActivity;
import androidx.media3.common.C;
import androidx.media3.common.Format;
import androidx.media3.common.MediaItem;
import androidx.media3.common.Player;
import androidx.media3.common.util.UnstableApi;
import androidx.media3.exoplayer.DecoderReuseEvaluation;
import androidx.media3.exoplayer.ExoPlayer;
import androidx.media3.exoplayer.analytics.AnalyticsListener;
import androidx.media3.exoplayer.source.LoadEventInfo;
import androidx.media3.exoplayer.source.MediaLoadData;
import androidx.media3.exoplayer.upstream.DefaultBandwidthMeter;
import androidx.media3.exoplayer.video.VideoFrameMetadataListener;
import androidx.media3.ui.PlayerView;

import com.google.android.material.button.MaterialButton;
import com.google.android.material.button.MaterialButtonToggleGroup;

import org.json.JSONArray;
import org.json.JSONObject;

import java.io.BufferedReader;
import java.io.InputStreamReader;
import java.net.HttpURLConnection;
import java.net.URL;
import java.util.ArrayList;
import java.util.List;
import java.util.Locale;
import java.util.UUID;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;

@OptIn(markerClass = UnstableApi.class)
public class MainActivity extends AppCompatActivity {

    private ExoPlayer player;
    private PlayerView playerView;
    private MaterialButton retryButton;
    private MaterialButton restartButton;
    private MaterialButton reloadButton;
    private MaterialButtonToggleGroup serverToggle;
    private MaterialButtonToggleGroup protocolToggle;
    private MaterialButtonToggleGroup segmentToggle;
    private MaterialButtonToggleGroup codecToggle;
    private MaterialButton contentButton;
    private MaterialButton discoverButton;
    private TextView statusText;
    private MaterialButton fullscreenButton;
    private View rightPanel;
    private View panelGuideline;
    private boolean isFullscreen = false;

    private String currentUrl = "";
    private final String playerId = UUID.randomUUID().toString();
    private PlaybackMetrics metrics;

    private static class ServerEnvironment {
        final String name;
        final String host;
        final String port;
        final String apiPort;

        ServerEnvironment(String name, String host, String port, String apiPort) {
            this.name = name;
            this.host = host;
            this.port = port;
            this.apiPort = apiPort;
        }

        String label() {
            return name;
        }
    }

    // Server list — fully user-managed, persisted in SharedPreferences.
    // No defaults are ever seeded; users add servers via the "+" discovery
    // button. Empty state shows a helpful hint instead.
    private final List<ServerEnvironment> servers = new ArrayList<>();

    private static final String SERVERS_PREFS = "servers";
    private static final String SERVERS_KEY = "list";
    private static final String SERVERS_ACTIVE_KEY = "active_index";

    private final String[] protocols = {"HLS", "DASH"};
    private final String[] segments = {"LL", "2s", "6s"};
    private final String[] codecs = {"Auto", "H264", "H265/HEVC", "AV1"};

    private static final int DEFAULT_SEGMENT_INDEX = 2; // 6s

    private final List<ContentItem> availableContent = new ArrayList<>();
    private String selectedContent = "";
    private final ExecutorService networkExecutor = Executors.newSingleThreadExecutor();
    private final Handler mainHandler = new Handler(Looper.getMainLooper());

    private static class ContentItem {
        final String name;
        final boolean hasHls;
        final boolean hasDash;

        ContentItem(String name, boolean hasHls, boolean hasDash) {
            this.name = name;
            this.hasHls = hasHls;
            this.hasDash = hasDash;
        }
    }

    private static String inferCodec(String name) {
        String lower = name.toLowerCase(Locale.US);
        if (lower.contains("av1")) return "AV1";
        if (lower.contains("hevc") || lower.contains("h265")) return "H265/HEVC";
        return "H264";
    }

    @Override
    protected void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);
        setContentView(R.layout.activity_main);

        playerView = findViewById(R.id.player_view);
        retryButton = findViewById(R.id.retry_button);
        restartButton = findViewById(R.id.restart_button);
        reloadButton = findViewById(R.id.reload_button);
        serverToggle = findViewById(R.id.server_toggle);
        protocolToggle = findViewById(R.id.protocol_toggle);
        segmentToggle = findViewById(R.id.segment_toggle);
        codecToggle = findViewById(R.id.codec_toggle);
        contentButton = findViewById(R.id.content_button);
        discoverButton = findViewById(R.id.discover_button);
        statusText = findViewById(R.id.status_text);
        fullscreenButton = findViewById(R.id.fullscreen_button);
        rightPanel = findViewById(R.id.right_panel);
        panelGuideline = findViewById(R.id.panel_guideline);

        fullscreenButton.setOnClickListener(v -> toggleFullscreen());

        styleActionButton(retryButton);
        styleActionButton(restartButton);
        styleActionButton(reloadButton);
        styleActionButton(contentButton);
        styleActionButton(fullscreenButton);
        styleActionButton(discoverButton);

        loadServers();
        initializePlayer();
        retryButton.requestFocus(); // start focus top-left of left panel
        setupToggles();

        retryButton.setOnClickListener(v -> retryFetch());
        restartButton.setOnClickListener(v -> restartPlayback());
        reloadButton.setOnClickListener(v -> {
            fetchContentList();
        });
        contentButton.setOnClickListener(v -> showContentPicker());
        discoverButton.setOnClickListener(v -> showAddServerMenu());
    }

    private void initializePlayer() {
        DefaultBandwidthMeter bandwidthMeter = new DefaultBandwidthMeter.Builder(this).build();
        player = new ExoPlayer.Builder(this)
            .setBandwidthMeter(bandwidthMeter)
            .build();
        playerView.setPlayer(player);

        metrics = new PlaybackMetrics(
            player,
            playerView,
            bandwidthMeter,
            playerId,
            () -> {
                ServerEnvironment s = currentServer();
                return "http://" + s.host + ":" + s.port;
            },
            () -> currentUrl
        );

        player.addListener(new Player.Listener() {
            @Override
            public void onPlayerError(androidx.media3.common.PlaybackException error) {
                statusText.setText("Error: " + error.getMessage());
                Toast.makeText(MainActivity.this, "Playback error: " + error.getMessage(),
                    Toast.LENGTH_LONG).show();
                metrics.onPlayerError(error.getMessage());
            }

            @Override
            public void onRenderedFirstFrame() {
                metrics.onFirstFrameRendered();
            }

            @Override
            public void onIsPlayingChanged(boolean isPlaying) {
                if (isPlaying) {
                    getWindow().addFlags(WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON);
                    metrics.onStallEnd();
                } else {
                    getWindow().clearFlags(WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON);
                }
            }

            @Override
            public void onPlaybackStateChanged(int state) {
                if (state == Player.STATE_BUFFERING
                    && player.getPlayWhenReady()
                    && !player.isPlaying()) {
                    metrics.onStallStart();
                }
            }
        });

        player.addAnalyticsListener(new AnalyticsListener() {
            @Override
            public void onDroppedVideoFrames(EventTime eventTime, int droppedFrames,
                                              long elapsedMs) {
                metrics.onDroppedFrames(droppedFrames);
            }

            @Override
            public void onVideoInputFormatChanged(EventTime eventTime, Format format,
                                                   DecoderReuseEvaluation decoderReuseEvaluation) {
                metrics.onVideoFormatChanged(format);
            }

            @Override
            public void onLoadCompleted(EventTime eventTime,
                                        LoadEventInfo loadEventInfo,
                                        MediaLoadData mediaLoadData) {
                if (mediaLoadData.trackType == C.TRACK_TYPE_VIDEO) {
                    metrics.onVideoLoadCompleted();
                }
            }
        });

        player.setVideoFrameMetadataListener(new VideoFrameMetadataListener() {
            @Override
            public void onVideoFrameAboutToBeRendered(long presentationTimeUs,
                                                      long releaseTimeNs,
                                                      Format format,
                                                      android.media.MediaFormat mediaFormat) {
                metrics.onFrameRendered();
            }
        });
    }

    private void setupToggles() {
        rebuildServerToggle();
        for (int i = 0; i < protocols.length; i++) {
            addToggleButton(protocolToggle, i, protocols[i]);
        }
        for (int i = 0; i < segments.length; i++) {
            addToggleButton(segmentToggle, i, segments[i]);
        }
        for (int i = 0; i < codecs.length; i++) {
            addToggleButton(codecToggle, i, codecs[i]);
        }

        // Only the leftmost button in each group escapes to the left panel
        for (MaterialButtonToggleGroup group :
                new MaterialButtonToggleGroup[]{ serverToggle, protocolToggle, segmentToggle, codecToggle }) {
            if (group.getChildCount() > 0) {
                group.getChildAt(0).setNextFocusLeftId(R.id.fullscreen_button);
            }
        }

        // Restore the server selected in the previous session, falling back
        // to the first entry. The seeded "Localhost" maps to index 0.
        int savedIdx = getSharedPreferences(SERVERS_PREFS, MODE_PRIVATE)
            .getInt(SERVERS_ACTIVE_KEY, 0);
        if (savedIdx < 0 || savedIdx >= serverToggle.getChildCount()) savedIdx = 0;
        if (serverToggle.getChildCount() > 0) {
            serverToggle.check(serverToggle.getChildAt(savedIdx).getId());
        }
        protocolToggle.check(protocolToggle.getChildAt(0).getId());
        segmentToggle.check(segmentToggle.getChildAt(DEFAULT_SEGMENT_INDEX).getId());
        codecToggle.check(codecToggle.getChildAt(0).getId());

        serverToggle.addOnButtonCheckedListener((group, checkedId, isChecked) -> {
            if (isChecked) {
                int idx = selectedIndex(serverToggle);
                if (idx >= 0) {
                    getSharedPreferences(SERVERS_PREFS, MODE_PRIVATE)
                        .edit().putInt(SERVERS_ACTIVE_KEY, idx).apply();
                }
                fetchContentList();
            }
        });
        protocolToggle.addOnButtonCheckedListener((group, checkedId, isChecked) -> {
            if (isChecked) applyContentFilter();
        });
        segmentToggle.addOnButtonCheckedListener((group, checkedId, isChecked) -> {
            if (isChecked) buildUrlAndLoad();
        });
        codecToggle.addOnButtonCheckedListener((group, checkedId, isChecked) -> {
            if (isChecked) applyContentFilter();
        });

        fetchContentList();
    }

    private void styleActionButton(MaterialButton button) {
        int[][] states = {
            { android.R.attr.state_focused },
            { -android.R.attr.state_focused }
        };
        button.setTextColor(new ColorStateList(states,
            new int[]{ 0xFFFFFFFF, 0xFFFFFFFF }));
        button.setBackgroundTintList(new ColorStateList(states,
            new int[]{ 0xFF1A3A4A, 0x00000000 }));
        button.setStrokeColor(new ColorStateList(states,
            new int[]{ 0xFF00B4D8, 0xFF555555 }));
    }

    private void addToggleButton(MaterialButtonToggleGroup group, int index, String text) {
        MaterialButton button = new MaterialButton(
            new androidx.appcompat.view.ContextThemeWrapper(this,
                com.google.android.material.R.style.Widget_MaterialComponents_Button_OutlinedButton),
            null, com.google.android.material.R.attr.materialButtonOutlinedStyle);
        button.setId(View.generateViewId());
        button.setText(text);
        button.setCheckable(true);
        button.setTextSize(android.util.TypedValue.COMPLEX_UNIT_SP, 12);

        int[][] states = {
            { android.R.attr.state_focused,  android.R.attr.state_checked },
            {                                android.R.attr.state_checked },
            { android.R.attr.state_focused, -android.R.attr.state_checked },
            {                               -android.R.attr.state_checked }
        };
        button.setTextColor(new ColorStateList(states,
            new int[]{ 0xFFFFFFFF, 0xFFFFFFFF, 0xFFFFFFFF, 0xFF888888 }));
        button.setBackgroundTintList(new ColorStateList(states,
            new int[]{ 0xFF00B4D8, 0xFF0077B6, 0xFF1A3A4A, 0x00000000 }));
        button.setStrokeColor(new ColorStateList(states,
            new int[]{ 0xFFFFFFFF, 0xFF00B4D8, 0xFF00B4D8, 0xFF444444 }));

        LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(
            ViewGroup.LayoutParams.WRAP_CONTENT,
            ViewGroup.LayoutParams.WRAP_CONTENT);
        button.setLayoutParams(lp);
        group.addView(button);
    }

    private int selectedIndex(MaterialButtonToggleGroup group) {
        int id = group.getCheckedButtonId();
        for (int i = 0; i < group.getChildCount(); i++) {
            if (group.getChildAt(i).getId() == id) return i;
        }
        return -1;
    }

    private ServerEnvironment currentServer() {
        int i = selectedIndex(serverToggle);
        if (i >= 0 && i < servers.size()) return servers.get(i);
        return servers.isEmpty() ? null : servers.get(0);
    }

    private void rebuildServerToggle() {
        serverToggle.removeAllViews();
        for (int i = 0; i < servers.size(); i++) {
            addToggleButton(serverToggle, i, servers.get(i).label());
            // Long-press a server to forget it. On Android TV remotes that's
            // a held OK / select. On a touchscreen it's a long-tap.
            final int idx = i;
            View btn = serverToggle.getChildAt(i);
            btn.setOnLongClickListener(v -> { confirmForgetServer(idx); return true; });
        }
        if (serverToggle.getChildCount() > 0) {
            serverToggle.getChildAt(0).setNextFocusLeftId(R.id.fullscreen_button);
        }
    }

    private void confirmForgetServer(int idx) {
        if (idx < 0 || idx >= servers.size()) return;
        ServerEnvironment s = servers.get(idx);
        new AlertDialog.Builder(this)
            .setTitle("Forget server")
            .setMessage("Remove \"" + s.label() + "\" from this device?")
            .setPositiveButton("Forget", (d, w) -> forgetServer(idx))
            .setNegativeButton("Cancel", null)
            .show();
    }

    private void forgetServer(int idx) {
        if (idx < 0 || idx >= servers.size()) return;
        boolean wasActive = (selectedIndex(serverToggle) == idx);
        servers.remove(idx);
        persistServers();
        rebuildServerToggle();
        if (servers.isEmpty()) {
            getSharedPreferences(SERVERS_PREFS, MODE_PRIVATE)
                .edit().remove(SERVERS_ACTIVE_KEY).apply();
            availableContent.clear();
            applyContentFilter();
            statusText.setText("No server selected. Tap \"+ Add server\" to discover one.");
        } else if (wasActive || serverToggle.getCheckedButtonId() == View.NO_ID) {
            serverToggle.check(serverToggle.getChildAt(0).getId());
        }
    }

    private void loadServers() {
        servers.clear();
        SharedPreferences prefs = getSharedPreferences(SERVERS_PREFS, MODE_PRIVATE);
        String json = prefs.getString(SERVERS_KEY, "");
        if (!json.isEmpty()) {
            try {
                JSONArray arr = new JSONArray(json);
                for (int i = 0; i < arr.length(); i++) {
                    JSONObject o = arr.getJSONObject(i);
                    servers.add(new ServerEnvironment(
                        o.optString("name", "Server " + i),
                        o.optString("host", ""),
                        o.optString("port", ""),
                        o.optString("apiPort", "")));
                }
            } catch (Exception ignored) { }
        }
    }

    private void persistServers() {
        JSONArray arr = new JSONArray();
        for (ServerEnvironment s : servers) {
            try {
                JSONObject o = new JSONObject();
                o.put("name", s.name);
                o.put("host", s.host);
                o.put("port", s.port);
                o.put("apiPort", s.apiPort);
                arr.put(o);
            } catch (Exception ignored) { }
        }
        SharedPreferences prefs = getSharedPreferences(SERVERS_PREFS, MODE_PRIVATE);
        prefs.edit().putString(SERVERS_KEY, arr.toString()).apply();
    }

    /**
     * Top-level "+ Add server" menu. Three ways to add a server:
     *  - Discover (same WAN, no code)
     *  - Pair with code (cross-network — TV shows code, user types it on a dashboard)
     *  - Enter manually (always works as a fallback)
     */
     private void showAddServerMenu() {
        // Kick off discovery immediately and surface results inline above
        // pair-with-code / manual-entry. 95% of the time the TV is on the
        // same network as the server and the user picks the first hit, so
        // the previous "Discover on this network…" sub-dialog was an extra
        // tap for the common case.
        statusText.setText(R.string.discovering);
        discoverButton.setEnabled(false);
        RendezvousService.discoverServers(this, (found, error) -> {
            discoverButton.setEnabled(true);
            if (error != null) {
                statusText.setText("Discovery failed: " + error);
            } else {
                statusText.setText("");
            }
            showAddServerMenuWith(found != null ? found : new ArrayList<>());
        });
    }

    private void showAddServerMenuWith(List<RendezvousService.DiscoveredServer> discovered) {
        final int n = discovered.size();
        String[] options = new String[n + 2];
        for (int i = 0; i < n; i++) {
            RendezvousService.DiscoveredServer s = discovered.get(i);
            options[i] = s.label + "\n" + s.url;
        }
        options[n] = "Pair with code…";
        options[n + 1] = "Enter host/port manually…";
        String title = n > 0
            ? "Add a server (" + n + " found)"
            : "Add a server (no servers detected)";
        new AlertDialog.Builder(this)
            .setTitle(title)
            .setItems(options, (dialog, which) -> {
                if (which < n) {
                    addDiscoveredServer(discovered.get(which));
                } else if (which == n) {
                    showPairWithCodeDialog();
                } else {
                    showManualEntryDialog();
                }
            })
            .setNegativeButton("Cancel", null)
            .show();
    }

    /**
     * Generates a pairing code, shows it to the user, and polls the
     * rendezvous Worker until a server URL appears (the user types this
     * code into a dashboard's "Pair with code" widget on another device).
     */
    private void showPairWithCodeDialog() {
        final String code = RendezvousService.generateCode();
        final TextView msgView = new TextView(this);
        msgView.setText(
            "On any device that can reach your server, open its dashboard's \"Server Info\" " +
            "panel and enter this code:\n\n" + code +
            "\n\nWaiting for the dashboard to publish the URL…"
        );
        msgView.setPadding(40, 30, 40, 30);
        msgView.setTextSize(16);

        AlertDialog dlg = new AlertDialog.Builder(this)
            .setTitle("Pair with code")
            .setView(msgView)
            .setNegativeButton("Cancel", null)
            .setCancelable(false)
            .create();
        dlg.show();

        // 2s poll, 10min timeout matches the iOS/tvOS PairingView defaults.
        RendezvousService.Cancel canceller = RendezvousService.pollForServerURL(
            this, code, 2000, 10L * 60L * 1000L,
            (serverURL, error) -> {
                if (!dlg.isShowing()) return;
                dlg.dismiss();
                if (error != null) {
                    Toast.makeText(this, "Pairing failed: " + error, Toast.LENGTH_LONG).show();
                    return;
                }
                addServerFromURL(serverURL);
            });
        dlg.setOnDismissListener(d -> canceller.cancel());
    }

    /**
     * Manual host/port entry as a final fallback. Defaults to API port
     * 30000 (matching the standard Docker Compose layout); playback port
     * is derived as content + 81 — same convention as the iOS app.
     */
    private void showManualEntryDialog() {
        LinearLayout container = new LinearLayout(this);
        container.setOrientation(LinearLayout.VERTICAL);
        container.setPadding(40, 20, 40, 20);

        EditText hostField = new EditText(this);
        hostField.setHint("hostname or IP (e.g. lenovo.local)");
        hostField.setSingleLine(true);
        container.addView(hostField);

        EditText portField = new EditText(this);
        portField.setHint("API port (default 30000)");
        portField.setInputType(android.text.InputType.TYPE_CLASS_NUMBER);
        portField.setSingleLine(true);
        container.addView(portField);

        new AlertDialog.Builder(this)
            .setTitle("Enter server")
            .setView(container)
            .setPositiveButton("Add", (dialog, which) -> {
                String host = hostField.getText().toString().trim();
                String portStr = portField.getText().toString().trim();
                int port = 30000;
                if (!portStr.isEmpty()) {
                    try { port = Integer.parseInt(portStr); }
                    catch (NumberFormatException ignored) {}
                }
                if (host.isEmpty() || port <= 0 || port >= 65536) {
                    Toast.makeText(this, "Invalid host or port", Toast.LENGTH_SHORT).show();
                    return;
                }
                addServerFromURL("http://" + host + ":" + port);
            })
            .setNegativeButton("Cancel", null)
            .show();
    }

    /** Common path used by pair-with-code and manual-entry to add a
     *  server given a "scheme://host:port" URL. */
    private void addServerFromURL(String urlString) {
        addDiscoveredServer(new RendezvousService.DiscoveredServer("manual-" + System.currentTimeMillis(), urlString, ""));
    }

    /**
     * Parse a discovered URL (e.g. "http://lenovo.local:30000") into a
     * ServerEnvironment, dedup against existing entries by host:port, persist,
     * and select it. Playback port = content port + 81 by convention (see
     * iOS ServerProfile.fromDashboardURL).
     */
    private void addDiscoveredServer(RendezvousService.DiscoveredServer disc) {
        Uri u = Uri.parse(disc.url);
        String host = u.getHost();
        int port = u.getPort();
        if (host == null || host.isEmpty()) {
            Toast.makeText(this, "Invalid URL: " + disc.url, Toast.LENGTH_LONG).show();
            return;
        }
        if (port < 0) port = ("https".equalsIgnoreCase(u.getScheme())) ? 443 : 80;
        String apiPort = String.valueOf(port);
        String playPort = String.valueOf(port + 81);
        // Use host:port so multiple servers from the same announce label
        // remain distinguishable on the button row.
        String name = host + ":" + port;

        for (int i = 0; i < servers.size(); i++) {
            ServerEnvironment ex = servers.get(i);
            if (ex.host.equalsIgnoreCase(host) && ex.apiPort.equals(apiPort)) {
                serverToggle.check(serverToggle.getChildAt(i).getId());
                Toast.makeText(this, "Already added: " + ex.label(), Toast.LENGTH_SHORT).show();
                return;
            }
        }
        ServerEnvironment added = new ServerEnvironment(name, host, playPort, apiPort);
        servers.add(added);
        persistServers();
        rebuildServerToggle();
        int newIdx = servers.size() - 1;
        serverToggle.check(serverToggle.getChildAt(newIdx).getId());
        Toast.makeText(this, "Added " + name, Toast.LENGTH_SHORT).show();
    }

    private String currentProtocol() {
        int i = selectedIndex(protocolToggle);
        return i >= 0 ? protocols[i] : protocols[0];
    }

    private String currentSegment() {
        int i = selectedIndex(segmentToggle);
        return i >= 0 ? segments[i] : segments[DEFAULT_SEGMENT_INDEX];
    }

    private String currentCodec() {
        int i = selectedIndex(codecToggle);
        return i >= 0 ? codecs[i] : codecs[0];
    }

    private void fetchContentList() {
        ServerEnvironment server = currentServer();
        if (server == null) {
            availableContent.clear();
            applyContentFilter();
            statusText.setText("No server selected. Tap \"+ Add server\" to discover one.");
            return;
        }
        final String urlStr = String.format(Locale.US, "http://%s:%s/api/content",
            server.host, server.apiPort);
        statusText.setText("Loading content list…");
        networkExecutor.execute(() -> {
            List<ContentItem> fetched = new ArrayList<>();
            String error = null;
            HttpURLConnection conn = null;
            try {
                URL url = new URL(urlStr);
                conn = (HttpURLConnection) url.openConnection();
                conn.setConnectTimeout(5000);
                conn.setReadTimeout(5000);
                int code = conn.getResponseCode();
                if (code != 200) {
                    error = "HTTP " + code + " from " + urlStr;
                } else {
                    StringBuilder sb = new StringBuilder();
                    try (BufferedReader r = new BufferedReader(new InputStreamReader(conn.getInputStream()))) {
                        String line;
                        while ((line = r.readLine()) != null) sb.append(line);
                    }
                    JSONArray arr = new JSONArray(sb.toString());
                    for (int i = 0; i < arr.length(); i++) {
                        JSONObject o = arr.getJSONObject(i);
                        fetched.add(new ContentItem(
                            o.getString("name"),
                            o.optBoolean("has_hls", false),
                            o.optBoolean("has_dash", false)));
                    }
                }
            } catch (Exception e) {
                error = e.getClass().getSimpleName() + ": " + e.getMessage();
            } finally {
                if (conn != null) conn.disconnect();
            }
            final String errMsg = error;
            mainHandler.post(() -> {
                if (errMsg != null) {
                    statusText.setText("Fetch failed: " + errMsg);
                    Toast.makeText(MainActivity.this, errMsg, Toast.LENGTH_LONG).show();
                    availableContent.clear();
                } else {
                    availableContent.clear();
                    availableContent.addAll(fetched);
                    statusText.setText("Loaded " + fetched.size() + " items from " + urlStr);
                }
                applyContentFilter();
            });
        });
    }

    private List<ContentItem> filteredContent() {
        String protocol = currentProtocol();
        String codec = currentCodec();
        List<ContentItem> out = new ArrayList<>();
        for (ContentItem c : availableContent) {
            boolean protocolOk = protocol.equals("HLS") ? c.hasHls : c.hasDash;
            if (!protocolOk) continue;
            if (!codec.equals("Auto") && !inferCodec(c.name).equals(codec)) continue;
            out.add(c);
        }
        return out;
    }

    private void applyContentFilter() {
        List<ContentItem> filtered = filteredContent();
        if (filtered.isEmpty()) {
            selectedContent = "";
            contentButton.setText(R.string.select_content);
            currentUrl = "";
            if (player != null) {
                player.stop();
                player.clearMediaItems();
            }
            return;
        }
        boolean stillValid = false;
        for (ContentItem c : filtered) {
            if (c.name.equals(selectedContent)) {
                stillValid = true;
                break;
            }
        }
        if (!stillValid) {
            selectedContent = filtered.get(0).name;
        }
        contentButton.setText(selectedContent);
        buildUrlAndLoad();
    }

    private void showContentPicker() {
        final List<ContentItem> filtered = filteredContent();
        if (filtered.isEmpty()) {
            Toast.makeText(this, "No content matches filters", Toast.LENGTH_SHORT).show();
            return;
        }
        String[] names = new String[filtered.size()];
        int preselect = 0;
        for (int i = 0; i < filtered.size(); i++) {
            names[i] = filtered.get(i).name;
            if (names[i].equals(selectedContent)) preselect = i;
        }
        new AlertDialog.Builder(this)
            .setTitle("Select Content (" + filtered.size() + ")")
            .setSingleChoiceItems(names, preselect, (dialog, which) -> {
                selectedContent = filtered.get(which).name;
                contentButton.setText(selectedContent);
                dialog.dismiss();
                buildUrlAndLoad();
            })
            .setNegativeButton("Cancel", null)
            .show();
    }

    private void buildUrlAndLoad() {
        if (selectedContent.isEmpty()) return;
        ServerEnvironment server = currentServer();
        String protocol = currentProtocol();
        String segment = currentSegment();
        String segmentSuffix = segment.equals("LL") ? "" : ("_" + segment.toLowerCase(Locale.US));
        String manifestFile = protocol.equals("HLS")
            ? ("master" + segmentSuffix + ".m3u8")
            : ("manifest" + segmentSuffix + ".mpd");
        currentUrl = String.format(Locale.US, "http://%s:%s/go-live/%s/%s?player_id=%s",
            server.host, server.port, selectedContent, manifestFile, playerId);
        loadStream(currentUrl);
    }

    private void loadStream(String url) {
        if (player != null && !url.isEmpty()) {
            // Target offset + min/max offsets stay UNSET so the manifest's
            // EXT-X-SERVER-CONTROL HOLD-BACK / PART-HOLD-BACK picks the start
            // point — same first-play position as Apple tvOS. Narrow the
            // playback-speed window so ExoPlayer catches up via rate
            // adjustment after a stall instead of seeking toward the live
            // edge, mirroring AVPlayer's automaticallyPreservesTimeOffsetFromLive.
            MediaItem.LiveConfiguration liveConfig = new MediaItem.LiveConfiguration.Builder()
                .setTargetOffsetMs(C.TIME_UNSET)
                .setMinOffsetMs(C.TIME_UNSET)
                .setMaxOffsetMs(C.TIME_UNSET)
                .setMinPlaybackSpeed(0.97f)
                .setMaxPlaybackSpeed(1.03f)
                .build();
            MediaItem mediaItem = new MediaItem.Builder()
                .setUri(url)
                .setLiveConfiguration(liveConfig)
                .build();
            player.setMediaItem(mediaItem);
            player.prepare();
            player.setPlayWhenReady(true);
            statusText.setText(url);
            if (metrics != null) metrics.onPlaybackStarted();
        }
    }

    private void retryFetch() {
        if (!currentUrl.isEmpty()) loadStream(currentUrl);
    }

    private void restartPlayback() {
        if (player != null) {
            if (metrics != null) metrics.onRestart("manual");
            player.stop();
            player.clearMediaItems();
            buildUrlAndLoad();
        }
    }

    private void toggleFullscreen() {
        isFullscreen = !isFullscreen;
        ConstraintLayout.LayoutParams params =
            (ConstraintLayout.LayoutParams) panelGuideline.getLayoutParams();
        params.guidePercent = isFullscreen ? 1.0f : 0.65f;
        panelGuideline.setLayoutParams(params);
        rightPanel.setVisibility(isFullscreen ? View.GONE : View.VISIBLE);
        fullscreenButton.setText(isFullscreen
            ? getString(R.string.exit_fullscreen)
            : getString(R.string.fullscreen));
    }

    @Override
    public void onBackPressed() {
        if (isFullscreen) {
            toggleFullscreen();
        } else {
            super.onBackPressed();
        }
    }

    @Override
    protected void onStart() {
        super.onStart();
        if (player != null) player.setPlayWhenReady(true);
        if (metrics != null) metrics.start();
    }

    @Override
    protected void onStop() {
        super.onStop();
        if (player != null) player.setPlayWhenReady(false);
        if (metrics != null) metrics.stop();
    }

    @Override
    protected void onDestroy() {
        super.onDestroy();
        if (metrics != null) {
            metrics.release();
            metrics = null;
        }
        if (player != null) {
            player.release();
            player = null;
        }
    }
}
