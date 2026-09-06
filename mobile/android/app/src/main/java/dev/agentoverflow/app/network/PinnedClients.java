package dev.agentoverflow.app.network;

import java.security.MessageDigest;
import java.security.cert.CertificateException;
import java.security.cert.X509Certificate;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.concurrent.TimeUnit;
import javax.net.ssl.SSLContext;
import javax.net.ssl.TrustManager;
import javax.net.ssl.X509TrustManager;
import okhttp3.ConnectionSpec;
import okhttp3.HttpUrl;
import okhttp3.OkHttpClient;
import okhttp3.Protocol;

/** The pairing fingerprint is the trust root, exactly as in Go deviceclient.
 * No CA fallback, hostname fallback, redirects, cookies or replay of POSTs.
 * An explicitly empty pin uses ordinary platform TLS verification for route
 * health probes; malformed/nonempty pins can never fall back to that path. */
public final class PinnedClients implements AutoCloseable {
    private final OkHttpClient base = new OkHttpClient.Builder()
            .followRedirects(false).followSslRedirects(false).retryOnConnectionFailure(false)
            .connectionSpecs(List.of(ConnectionSpec.MODERN_TLS)).protocols(List.of(Protocol.HTTP_1_1))
            .connectTimeout(20, TimeUnit.SECONDS).readTimeout(60, TimeUnit.SECONDS)
            .writeTimeout(60, TimeUnit.SECONDS).build();
    private final LinkedHashMap<String, OkHttpClient> clients = new LinkedHashMap<>(16, .75f, true);

    public static HttpUrl endpoint(String value) {
        HttpUrl url = HttpUrl.get(value.replaceFirst("^wss:", "https:"));
        if (!url.isHttps() || !url.username().isEmpty() || !url.password().isEmpty() || url.fragment() != null) {
            throw new IllegalArgumentException("LAN connections require a paired HTTPS address");
        }
        return url;
    }

    public synchronized OkHttpClient forPin(String fingerprint) throws Exception {
        if ("".equals(fingerprint)) return base;
        if (fingerprint == null || !fingerprint.matches("sha256:[0-9a-f]{64}")) {
            throw new IllegalArgumentException("Invalid pairing certificate fingerprint");
        }
        OkHttpClient existing = clients.get(fingerprint);
        if (existing != null) return existing;
        // HexFormat is newer than the Android API floor; decode explicitly.
        byte[] expected = new byte[32];
        for (int i = 0; i < expected.length; i++) {
            expected[i] = (byte) Integer.parseInt(fingerprint.substring(7 + i * 2, 9 + i * 2), 16);
        }
        X509TrustManager trust = new X509TrustManager() {
            public X509Certificate[] getAcceptedIssuers() { return new X509Certificate[0]; }
            public void checkClientTrusted(X509Certificate[] chain, String type) throws CertificateException {
                throw new CertificateException("Client certificates are not accepted");
            }
            public void checkServerTrusted(X509Certificate[] chain, String type) throws CertificateException {
                try {
                    if (chain == null || chain.length == 0 || !MessageDigest.isEqual(expected,
                            MessageDigest.getInstance("SHA-256").digest(chain[0].getEncoded()))) {
                        throw new CertificateException("The computer's certificate changed. Pair it again to verify its identity.");
                    }
                } catch (CertificateException e) { throw e; }
                catch (Exception e) { throw new CertificateException(e); }
            }
        };
        SSLContext tls = SSLContext.getInstance("TLS");
        tls.init(null, new TrustManager[] { trust }, null);
        OkHttpClient client = base.newBuilder().sslSocketFactory(tls.getSocketFactory(), trust)
                // Identity was verified by the exact leaf above; LAN IPs can change.
                .hostnameVerifier((hostname, session) -> true).build();
        clients.put(fingerprint, client);
        if (clients.size() > 16) clients.remove(clients.keySet().iterator().next());
        return client;
    }

    @Override public void close() {
        base.dispatcher().cancelAll();
        base.connectionPool().evictAll();
        clients.clear();
    }
}
