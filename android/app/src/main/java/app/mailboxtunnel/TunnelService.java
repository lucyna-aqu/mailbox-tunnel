package app.mailboxtunnel;

import android.content.Intent;
import android.content.pm.PackageManager;
import android.net.VpnService;
import android.os.ParcelFileDescriptor;
import android.util.Log;

import app.mailboxtunnel.mobile.Mobile;

/** Routes the device's traffic through the tunnel. The app itself is left out so the tunnel's own connections go direct. */
public class TunnelService extends VpnService {
    static final String ACTION_STOP = "app.mailboxtunnel.STOP";
    private static final String TAG = "TunnelService";

    private ParcelFileDescriptor tun;

    @Override
    public int onStartCommand(Intent intent, int flags, int startId) {
        Mobile.setDataDir(getFilesDir().getAbsolutePath());
        if (intent != null && ACTION_STOP.equals(intent.getAction())) {
            shutDown();
            return START_NOT_STICKY;
        }
        if (tun != null) {
            return START_STICKY;
        }
        try {
            tun = new Builder()
                    .setSession("mailbox-tunnel")
                    .setMtu(1500)
                    .addAddress("10.111.0.2", 32)
                    .addRoute("0.0.0.0", 0)
                    .addAddress("fd00:111::2", 128)
                    .addRoute("::", 0)
                    .addDnsServer("1.1.1.1")
                    .addDisallowedApplication(getPackageName())
                    .establish();
        } catch (PackageManager.NameNotFoundException | IllegalArgumentException | IllegalStateException e) {
            Log.e(TAG, "could not set up the VPN interface", e);
            stopSelf();
            return START_NOT_STICKY;
        }
        if (tun == null) {
            stopSelf();
            return START_NOT_STICKY;
        }
        String error = Mobile.start(tun.detachFd());
        if (!error.isEmpty()) {
            Log.e(TAG, error);
        }
        return START_STICKY;
    }

    @Override
    public void onRevoke() {
        shutDown();
    }

    @Override
    public void onDestroy() {
        new Thread(Mobile::stop).start();
        super.onDestroy();
    }

    private void shutDown() {
        // Go owns the file descriptor after detachFd and closes it when it stops.
        tun = null;
        new Thread(Mobile::stop).start();
        stopSelf();
    }
}
