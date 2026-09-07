package dev.agentoverflow.app.network;

import static org.junit.Assert.*;
import java.io.ByteArrayOutputStream;
import java.security.MessageDigest;
import java.util.Map;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.TimeUnit;
import mockwebserver3.MockResponse;
import mockwebserver3.MockWebServer;
import okhttp3.Request;
import okhttp3.Response;
import okhttp3.WebSocket;
import okhttp3.WebSocketListener;
import okhttp3.tls.HandshakeCertificates;
import okhttp3.tls.HeldCertificate;
import org.junit.Test;

public class PinnedNetworkTest {
    @Test public void ordinaryTrustNeverAcceptsAnUntrustedPrivateCertificate() throws Exception {
        HeldCertificate cert = new HeldCertificate.Builder().commonName("localhost").addSubjectAlternativeName("localhost").build();
        try (MockWebServer server = server(cert); PinnedClients clients = new PinnedClients()) {
            assertThrows(java.io.IOException.class, () -> clients.forPin("").newCall(new Request.Builder()
                    .url(server.url("/healthz")).header("X-AO-Session", "must-not-arrive").build()).execute());
            assertEquals(0, server.getRequestCount());
            assertThrows(IllegalArgumentException.class, () -> clients.forPin(null));
            assertThrows(IllegalArgumentException.class, () -> clients.forPin("sha256:broken"));
        }
    }
    private static String pin(HeldCertificate cert) throws Exception {
        byte[] digest = MessageDigest.getInstance("SHA-256").digest(cert.certificate().getEncoded());
        StringBuilder text = new StringBuilder("sha256:");
        for (byte b : digest) text.append(String.format("%02x", b));
        return text.toString();
    }

    private static MockWebServer server(HeldCertificate cert) throws Exception {
        MockWebServer server = new MockWebServer();
        server.useHttps(new HandshakeCertificates.Builder().heldCertificate(cert).build().sslSocketFactory());
        server.start();
        return server;
    }

    @Test public void exactLeafAuthenticatesEvenWhenAddressIsNotInTheCertificate() throws Exception {
        HeldCertificate cert = new HeldCertificate.Builder().commonName("this-computer").build();
        try (MockWebServer server = server(cert); PinnedClients clients = new PinnedClients()) {
            server.enqueue(new MockResponse.Builder().body("paired").build());
            try (Response response = clients.forPin(pin(cert)).newCall(new Request.Builder().url(server.url("/bootstrap.json")).build()).execute()) {
                assertEquals("paired", response.body().string());
            }
        }
    }

    @Test public void wrongLeafIsRefusedBeforeAnyCredentialCrosses() throws Exception {
        HeldCertificate cert = new HeldCertificate.Builder().commonName("localhost").addSubjectAlternativeName("localhost").build();
        try (MockWebServer server = server(cert); PinnedClients clients = new PinnedClients()) {
            try {
                clients.forPin("sha256:" + "0".repeat(64)).newCall(new Request.Builder().url(server.url("/auth/token"))
                        .header("X-AO-Session", "must-not-arrive").build()).execute();
                fail("accepted the wrong certificate");
            } catch (java.io.IOException expected) {
                assertTrue(NetworkPlugin.failureMessage(expected).contains("certificate"));
            }
            assertEquals(0, server.getRequestCount());
        }
    }

    @Test public void anEarlyFailureRemainsReadableByTheHeaderConsumer() throws Exception {
        HeldCertificate cert = new HeldCertificate.Builder().commonName("localhost").build();
        try (MockWebServer server = server(cert); PinnedClients clients = new PinnedClients(); HttpStreams http = new HttpStreams(clients)) {
            var transfer = http.start("bad-pin", server.url("/auth/token").toString(), "sha256:" + "0".repeat(64), "POST", Map.of(), 2);
            assertThrows(java.util.concurrent.ExecutionException.class, () -> transfer.headers.get(10, TimeUnit.SECONDS));
            // The request can fail before the bridge even asks for its headers.
            var failure = assertThrows(java.util.concurrent.ExecutionException.class, () -> http.get("bad-pin").headers.get());
            assertTrue(NetworkPlugin.failureMessage(failure).contains("certificate"));
            // POST bodies cross the bridge before JS reads headers. A TLS
            // refusal must survive that ordering too, not become "closed".
            var writeFailure = assertThrows(java.io.IOException.class,
                    () -> http.get("bad-pin").write(new byte[] {1, 2}, true));
            assertTrue(NetworkPlugin.failureMessage(writeFailure).contains("certificate could not be verified"));
            assertEquals(0, server.getRequestCount());
            http.close("bad-pin");
            assertThrows(java.io.IOException.class, () -> http.get("bad-pin"));
        }
    }

    @Test public void redirectsDoNotForwardTicketsOrCredentials() throws Exception {
        HeldCertificate cert = new HeldCertificate.Builder().commonName("test").build();
        try (MockWebServer server = server(cert); PinnedClients clients = new PinnedClients()) {
            server.enqueue(new MockResponse.Builder().code(307).addHeader("Location", server.url("/stolen")).build());
            try (Response response = clients.forPin(pin(cert)).newCall(new Request.Builder().url(server.url("/attachments/upload?ticket=secret")).build()).execute()) {
                assertEquals(307, response.code());
            }
            assertEquals(1, server.getRequestCount());
        }
    }

    @Test public void largeBodiesStreamIndependentlyAndCloseReclaimsTheHandle() throws Exception {
        HeldCertificate cert = new HeldCertificate.Builder().commonName("test").build();
        try (MockWebServer server = server(cert); PinnedClients clients = new PinnedClients(); HttpStreams http = new HttpStreams(clients)) {
            String content = "0123456789abcdef".repeat(131072);
            server.enqueue(new MockResponse.Builder().body(content).build());
            var transfer = http.start("upload", server.url("/attachments/upload").toString(), pin(cert), "PUT",
                    Map.of("content-type", "text/plain", "cookie", "must-not-arrive"), content.length());
            byte[] chunk = content.substring(0, UploadPipe.CHUNK_BYTES).getBytes(java.nio.charset.StandardCharsets.UTF_8);
            for (int at = 0; at < content.length(); at += chunk.length) transfer.write(chunk, at + chunk.length == content.length());
            assertEquals(200, transfer.headers.get(10, TimeUnit.SECONDS).code());
            var request = server.takeRequest(10, TimeUnit.SECONDS);
            assertNotNull(request);
            assertNull(request.getHeaders().get("Cookie"));
            assertEquals(content.length(), request.getBodySize());
            ByteArrayOutputStream read = new ByteArrayOutputStream();
            for (byte[] bytes; (bytes = transfer.read()).length > 0;) {
                assertTrue(bytes.length <= UploadPipe.CHUNK_BYTES);
                read.write(bytes);
            }
            assertEquals(content, read.toString(java.nio.charset.StandardCharsets.UTF_8));
            http.close("upload");
            assertThrows(java.io.IOException.class, () -> http.get("upload"));
        }
    }

    @Test public void aTicketPostWithoutAnExplicitBodyStillSendsAnEmptyPost() throws Exception {
        HeldCertificate cert = new HeldCertificate.Builder().commonName("test").build();
        try (MockWebServer server = server(cert); PinnedClients clients = new PinnedClients(); HttpStreams http = new HttpStreams(clients)) {
            server.enqueue(new MockResponse.Builder().body("ticket").build());
            var transfer = http.start("ticket", server.url("/auth/ticket").toString(), pin(cert), "POST", Map.of(), -1);
            assertEquals(200, transfer.headers.get(10, TimeUnit.SECONDS).code());
            var request = server.takeRequest(10, TimeUnit.SECONDS);
            assertNotNull(request);
            assertEquals("POST", request.getMethod());
            assertEquals(0, request.getBodySize());
        }
    }

    @Test public void cancellationUnblocksAnUploadWaitingForTheBridge() throws Exception {
        UploadPipe pipe = new UploadPipe(2, "text/plain");
        var reader = CompletableFuture.runAsync(() -> {
            try { pipe.writeTo(new okio.Buffer()); fail("cancelled upload completed"); }
            catch (java.io.IOException expected) { }
        });
        pipe.write(new byte[] {1}, false);
        pipe.cancel(new java.io.IOException("Transfer closed"));
        reader.get(2, TimeUnit.SECONDS);
        assertThrows(java.io.IOException.class, () -> pipe.write(new byte[] {2}, true));
    }

    @Test public void pinnedWebSocketCarriesTheNormalWireFrames() throws Exception {
        HeldCertificate cert = new HeldCertificate.Builder().commonName("test").build();
        try (MockWebServer server = server(cert); PinnedClients clients = new PinnedClients()) {
            server.enqueue(new MockResponse.Builder().webSocketUpgrade(new WebSocketListener() {
                @Override public void onMessage(WebSocket ws, String text) { ws.send(text); }
                @Override public void onClosing(WebSocket ws, int code, String reason) { ws.close(code, reason); }
            }).build());
            CompletableFuture<String> received = new CompletableFuture<>();
            CompletableFuture<Void> closed = new CompletableFuture<>();
            WebSocket ws = clients.forPin(pin(cert)).newWebSocket(new Request.Builder().url(server.url("/ws?ticket=paired")).build(), new WebSocketListener() {
                @Override public void onOpen(WebSocket socket, Response response) { socket.send("{\"method\":\"test\"}"); }
                @Override public void onMessage(WebSocket socket, String text) { received.complete(text); socket.close(1000, ""); }
                @Override public void onFailure(WebSocket socket, Throwable failure, Response response) { received.completeExceptionally(failure); closed.completeExceptionally(failure); }
                @Override public void onClosed(WebSocket socket, int code, String reason) { closed.complete(null); }
            });
            try {
                assertEquals("{\"method\":\"test\"}", received.get(10, TimeUnit.SECONDS));
                closed.get(10, TimeUnit.SECONDS);
            }
            finally { ws.cancel(); }
        }
    }
}
