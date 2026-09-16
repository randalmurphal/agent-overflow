package dev.agentoverflow.app;

import static org.mockito.ArgumentMatchers.*;
import static org.mockito.Mockito.*;

import android.webkit.WebView;
import android.net.Uri;
import androidx.webkit.JavaScriptReplyProxy;
import androidx.webkit.WebMessageCompat;
import androidx.webkit.WebViewCompat;
import androidx.webkit.WebViewFeature;
import com.getcapacitor.Bridge;
import com.getcapacitor.CapConfig;
import com.getcapacitor.JSObject;
import com.getcapacitor.MessageHandler;
import com.getcapacitor.PluginCall;
import java.util.Set;
import org.junit.Test;
import org.mockito.ArgumentCaptor;
import org.mockito.MockedStatic;

public class BridgeReplyTest {
    @Test public void immediateReplyUsesTheCurrentPageAfterReload() {
        Bridge bridge = mock(Bridge.class);
        CapConfig config = mock(CapConfig.class);
        when(bridge.getConfig()).thenReturn(config);
        when(bridge.getAllowedOriginRules()).thenReturn(Set.of("https://shell.agent-overflow.invalid"));
        WebView webView = mock(WebView.class);
        try (MockedStatic<Uri> uris = mockStatic(Uri.class);
             MockedStatic<WebViewFeature> features = mockStatic(WebViewFeature.class);
             MockedStatic<WebViewCompat> compat = mockStatic(WebViewCompat.class)) {
            features.when(() -> WebViewFeature.isFeatureSupported(WebViewFeature.WEB_MESSAGE_LISTENER)).thenReturn(true);
            new MessageHandler(bridge, webView, null) {
                @Override public void postMessage(String value) {
                    // A worker may complete before the dispatching callback returns.
                    new PluginCall(this, "Network", value, "getCapabilities", new JSObject()).resolve(new JSObject());
                }
            };
            ArgumentCaptor<WebViewCompat.WebMessageListener> listener = ArgumentCaptor.forClass(WebViewCompat.WebMessageListener.class);
            compat.verify(() -> WebViewCompat.addWebMessageListener(eq(webView), eq("androidBridge"), anySet(), listener.capture()));
            JavaScriptReplyProxy first = mock(JavaScriptReplyProxy.class);
            JavaScriptReplyProxy reloaded = mock(JavaScriptReplyProxy.class);
            WebMessageCompat message = mock(WebMessageCompat.class);
            when(message.getData()).thenReturn("first", "reloaded");
            listener.getValue().onPostMessage(webView, message, null, true, first);
            verify(first).postMessage(contains("\"callbackId\":\"first\""));
            listener.getValue().onPostMessage(webView, message, null, true, reloaded);
            verify(reloaded).postMessage(contains("\"callbackId\":\"reloaded\""));
            verifyNoMoreInteractions(first, reloaded);
        }
    }
}
