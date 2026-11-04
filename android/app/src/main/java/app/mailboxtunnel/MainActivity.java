package app.mailboxtunnel;

import android.app.Activity;
import android.content.Intent;
import android.net.VpnService;
import android.os.Bundle;
import android.webkit.JavascriptInterface;
import android.webkit.WebSettings;
import android.webkit.WebView;

import org.json.JSONArray;
import org.json.JSONException;
import org.json.JSONObject;

import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;

import app.mailboxtunnel.mobile.Mobile;

/** Shows the shared web interface and connects its calls to the Go core. */
public class MainActivity extends Activity {
    private static final int VPN_REQUEST = 1;

    private final ExecutorService worker = Executors.newCachedThreadPool();
    private WebView webView;

    @Override
    protected void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);
        Mobile.setDataDir(getFilesDir().getAbsolutePath());

        webView = new WebView(this);
        WebSettings settings = webView.getSettings();
        settings.setJavaScriptEnabled(true);
        webView.setBackgroundColor(0xFF101C24);
        webView.addJavascriptInterface(new Bridge(), "AndroidBridge");
        webView.loadUrl("file:///android_asset/web/index.html");
        setContentView(webView);

        if (savedInstanceState == null) {
            try {
                JSONObject state = new JSONObject(Mobile.state());
                if (state.optBoolean("configured") && state.optBoolean("autoConnect") && "disconnected".equals(state.optString("status"))) {
                    connect();
                }
            } catch (JSONException ignored) {
                // A broken state only means no automatic connect.
            }
        }
    }

    @Override
    protected void onActivityResult(int requestCode, int resultCode, Intent data) {
        super.onActivityResult(requestCode, resultCode, data);
        if (requestCode == VPN_REQUEST && resultCode == RESULT_OK) {
            startService(new Intent(this, TunnelService.class));
        }
    }

    private void connect() {
        Intent consent = VpnService.prepare(this);
        if (consent != null) {
            startActivityForResult(consent, VPN_REQUEST);
        } else {
            startService(new Intent(this, TunnelService.class));
        }
    }

    private void disconnect() {
        startService(new Intent(this, TunnelService.class).setAction(TunnelService.ACTION_STOP));
    }

    private void resolve(int id, String result) {
        String script = "window.__bridgeResolve(" + id + "," + JSONObject.quote(result) + ")";
        webView.post(() -> webView.evaluateJavascript(script, null));
    }

    /** Called from bridge.js. Every call returns at once; the result arrives through window.__bridgeResolve. */
    private class Bridge {
        @JavascriptInterface
        public void call(int id, String method, String argumentsJson) {
            worker.execute(() -> {
                try {
                    resolve(id, run(method, new JSONArray(argumentsJson)));
                } catch (JSONException e) {
                    resolve(id, "");
                }
            });
        }

        private String run(String method, JSONArray arguments) throws JSONException {
            switch (method) {
                case "State":
                    return Mobile.state();
                case "Traffic":
                    return Mobile.traffic();
                case "Logs":
                    return Mobile.logs();
                case "SaveSettings":
                    return Mobile.saveSettings(arguments.getString(0), arguments.getString(1));
                case "SetupCode":
                    return Mobile.setupCode();
                case "FindRoute":
                    return Mobile.findRoute();
                case "ResetSettings":
                    disconnect();
                    Mobile.resetSettings();
                    return "";
                case "SetAutoConnect":
                    Mobile.setAutoConnect(arguments.getBoolean(0));
                    return "";
                case "Connect":
                    runOnUiThread(MainActivity.this::connect);
                    return "";
                case "Disconnect":
                    runOnUiThread(MainActivity.this::disconnect);
                    return "";
                default:
                    return "";
            }
        }
    }
}
