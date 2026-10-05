package dev.agentoverflow.app.network;

import static org.junit.Assert.*;
import java.security.MessageDigest;
import java.util.Map;
import java.util.concurrent.ExecutionException;
import java.util.concurrent.TimeUnit;
import mockwebserver3.MockResponse;
import mockwebserver3.MockWebServer;
import okhttp3.OkHttpClient;
import okhttp3.Request;
import okhttp3.Response;
import okhttp3.tls.HandshakeCertificates;
import okhttp3.tls.HeldCertificate;
import org.junit.Test;

/** Idle connections may share a path that died without a close, so they are
 * kept only for a burst and dropped when a request fails. */
public class PinnedPoolTest {
    private static final HeldCertificate CERT = new HeldCertificate.Builder().commonName("this-computer").build();

    @Test public void connectionsAreReusedWithinABurstOnly() throws Exception {
        try (MockWebServer server = server(); PinnedClients clients = new PinnedClients(100)) {
            OkHttpClient client = clients.forPin(pin());
            get(client, server);
            get(client, server);
            int first = connection(server);
            assertEquals("a burst shares one connection", first, connection(server));
            Thread.sleep(500);
            get(client, server);
            assertNotEquals("a connection outlived its idle window", first, connection(server));
        }
    }

    @Test public void aFailedRequestLeavesNoIdleConnection() throws Exception {
        try (MockWebServer server = server(); PinnedClients clients = new PinnedClients(); HttpStreams http = new HttpStreams(clients)) {
            OkHttpClient client = clients.forPin(pin());
            get(client, server);
            int idle = connection(server);
            HttpStreams.Transfer failed = http.start("refused", server.url("/x").toString(), "sha256:" + "0".repeat(64), "GET", Map.of(), -1);
            assertThrows(ExecutionException.class, () -> failed.headers.get(10, TimeUnit.SECONDS));
            get(client, server);
            assertNotEquals("the request after a failure reused an idle connection", idle, connection(server));
        }
    }

    private static void get(OkHttpClient client, MockWebServer server) throws Exception {
        server.enqueue(new MockResponse.Builder().body("ok").build());
        try (Response response = client.newCall(new Request.Builder().url(server.url("/ok")).build()).execute()) {
            assertEquals("ok", response.body().string());
        }
    }

    private static int connection(MockWebServer server) throws Exception {
        return server.takeRequest(10, TimeUnit.SECONDS).getConnectionIndex();
    }

    private static String pin() throws Exception {
        byte[] digest = MessageDigest.getInstance("SHA-256").digest(CERT.certificate().getEncoded());
        StringBuilder text = new StringBuilder("sha256:");
        for (byte b : digest) text.append(String.format("%02x", b));
        return text.toString();
    }

    private static MockWebServer server() throws Exception {
        MockWebServer server = new MockWebServer();
        server.useHttps(new HandshakeCertificates.Builder().heldCertificate(CERT).build().sslSocketFactory());
        server.start();
        return server;
    }
}
