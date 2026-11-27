package app.mailboxtunnel;

import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.content.Intent;
import android.content.pm.PackageManager;
import android.content.pm.ServiceInfo;
import android.net.VpnService;
import android.os.Build;
import android.os.ParcelFileDescriptor;
import android.util.Log;

import app.mailboxtunnel.mobile.Mobile;

/** Routes the device's traffic through the tunnel. The app itself is left out so the tunnel's own connections go direct. */
public class TunnelService extends VpnService {
    static final String ACTION_STOP = "app.mailboxtunnel.STOP";
    private static final String TAG = "TunnelService";
    private static final String CHANNEL_ID = "mailbox_tunnel_vpn";
    private static final int NOTIFICATION_ID = 1;

    private volatile boolean running;

    @Override
    public int onStartCommand(Intent intent, int flags, int startId) {
        Mobile.setDataDir(getFilesDir().getAbsolutePath());
        if (intent != null && ACTION_STOP.equals(intent.getAction())) {
            shutDown();
            return START_NOT_STICKY;
        }
        if (running) {
            return START_STICKY;
        }
        startForegroundNotification();
        ParcelFileDescriptor tun;
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
            shutDown();
            return START_NOT_STICKY;
        }
        if (tun == null) {
            shutDown();
            return START_NOT_STICKY;
        }
        running = true;
        // Start may first wait for an earlier connection to stop, so it stays off the main thread.
        int fd = tun.detachFd();
        new Thread(() -> {
            String error = Mobile.start(fd);
            if (!error.isEmpty()) {
                Log.e(TAG, error);
                shutDown();
            }
        }).start();
        return START_STICKY;
    }

    private void startForegroundNotification() {
        Notification.Builder builder;
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            NotificationChannel channel = new NotificationChannel(
                    CHANNEL_ID,
                    "Mailbox Tunnel",
                    NotificationManager.IMPORTANCE_LOW
            );
            NotificationManager manager = getSystemService(NotificationManager.class);
            if (manager != null) {
                manager.createNotificationChannel(channel);
            }
            builder = new Notification.Builder(this, CHANNEL_ID);
        } else {
            builder = new Notification.Builder(this);
        }
        Intent openIntent = new Intent(this, MainActivity.class);
        PendingIntent contentIntent = PendingIntent.getActivity(
                this, 0, openIntent, PendingIntent.FLAG_IMMUTABLE | PendingIntent.FLAG_UPDATE_CURRENT
        );
        Notification notification = builder
                .setSmallIcon(R.mipmap.ic_launcher)
                .setContentTitle("Mailbox Tunnel")
                .setContentText("VPN active")
                .setOngoing(true)
                .setContentIntent(contentIntent)
                .build();
        if (Build.VERSION.SDK_INT >= 34) {
            startForeground(NOTIFICATION_ID, notification, ServiceInfo.FOREGROUND_SERVICE_TYPE_SPECIAL_USE);
        } else {
            startForeground(NOTIFICATION_ID, notification);
        }
    }

    @Override
    public void onRevoke() {
        shutDown();
    }

    @Override
    public void onDestroy() {
        running = false;
        new Thread(Mobile::stop).start();
        super.onDestroy();
    }

    private void shutDown() {
        // Go owns the file descriptor after detachFd and closes it when it stops.
        running = false;
        stopForeground(STOP_FOREGROUND_REMOVE);
        new Thread(Mobile::stop).start();
        stopSelf();
    }
}
