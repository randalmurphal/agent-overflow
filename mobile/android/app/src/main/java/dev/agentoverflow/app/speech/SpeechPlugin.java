package dev.agentoverflow.app.speech;

import android.os.Bundle;
import android.speech.tts.TextToSpeech;
import android.speech.tts.UtteranceProgressListener;
import android.speech.tts.Voice;

import com.getcapacitor.JSArray;
import com.getcapacitor.JSObject;
import com.getcapacitor.Plugin;
import com.getcapacitor.PluginCall;
import com.getcapacitor.PluginMethod;
import com.getcapacitor.annotation.CapacitorPlugin;

import org.json.JSONException;

import java.util.ArrayList;
import java.util.List;
import java.util.Locale;
import java.util.Set;

/**
 * Android's TextToSpeech for read-aloud ({@code frontend/src/lib/native/speech.ts}).
 *
 * <p>Android's WebView has no {@code speechSynthesis}, so the phone reads
 * replies through the system engine instead. Every piece of a reading is
 * queued at once, so a reading carries on while the app is in the
 * background, when the page's timers do not run.
 *
 * <p>The engine starts asynchronously; calls that arrive before it is ready
 * wait for it, and fail with the reason when it cannot start.
 */
@CapacitorPlugin(name = "Speech")
public class SpeechPlugin extends Plugin {

    private final SpeechSessions sessions = new SpeechSessions();
    private final List<Runnable> waiting = new ArrayList<>();
    private TextToSpeech tts;
    /** Null while starting; then whether the engine started. */
    private Boolean ready;

    @Override
    public void load() {
        tts = new TextToSpeech(getContext(), status -> {
            List<Runnable> run;
            synchronized (waiting) {
                ready = status == TextToSpeech.SUCCESS;
                run = new ArrayList<>(waiting);
                waiting.clear();
            }
            for (Runnable call : run) {
                call.run();
            }
        });
        tts.setOnUtteranceProgressListener(new UtteranceProgressListener() {
            @Override
            public void onStart(String utteranceId) {
                send(sessions.started(utteranceId));
            }

            @Override
            public void onDone(String utteranceId) {
                send(sessions.finished(utteranceId));
            }

            @Override
            @SuppressWarnings("deprecation")
            public void onError(String utteranceId) {
                send(sessions.failed(utteranceId, "the speech engine failed"));
            }

            @Override
            public void onError(String utteranceId, int errorCode) {
                send(sessions.failed(utteranceId, "the speech engine failed (" + errorCode + ")"));
            }
        });
    }

    @Override
    protected void handleOnDestroy() {
        if (tts != null) {
            tts.stop();
            tts.shutdown();
        }
    }

    /** Runs {@code call} once the engine has started, or rejects it. */
    private void whenReady(PluginCall pluginCall, Runnable call) {
        synchronized (waiting) {
            if (ready == null) {
                waiting.add(() -> whenReady(pluginCall, call));
                return;
            }
        }
        if (!ready) {
            pluginCall.reject("this phone's text-to-speech engine did not start");
            return;
        }
        call.run();
    }

    @PluginMethod
    public void voices(PluginCall call) {
        whenReady(call, () -> {
            JSArray list = new JSArray();
            Set<Voice> voices = tts.getVoices();
            Voice fallback = tts.getDefaultVoice();
            if (voices != null) {
                for (Voice voice : voices) {
                    if (voice.getFeatures().contains(TextToSpeech.Engine.KEY_FEATURE_NOT_INSTALLED)) {
                        continue;
                    }
                    JSObject entry = new JSObject();
                    entry.put("id", voice.getName());
                    entry.put("name", voice.getLocale().getDisplayName(Locale.getDefault()) + " (" + voice.getName() + ")");
                    entry.put("lang", voice.getLocale().toLanguageTag());
                    entry.put("isDefault", voice.equals(fallback));
                    list.put(entry);
                }
            }
            JSObject result = new JSObject();
            result.put("voices", list);
            call.resolve(result);
        });
    }

    @PluginMethod
    public void speak(PluginCall call) {
        String session = call.getString("session");
        JSArray pieces = call.getArray("pieces");
        int from = call.getInt("from", 0);
        if (session == null || session.isEmpty() || pieces == null) {
            call.reject("speak needs a session and its pieces");
            return;
        }
        String voiceId = call.getString("voiceId", "");
        float rate = call.getFloat("rate", 1f);
        float volume = call.getFloat("volume", 1f);
        whenReady(call, () -> {
            List<String> text;
            try {
                text = pieces.toList();
            } catch (JSONException malformed) {
                call.reject("speak pieces must be text", malformed);
                return;
            }
            if (from < 0 || from >= text.size()) {
                call.reject("speak starts past its last piece");
                return;
            }
            tts.stop();
            sessions.begin(session, text.size() - 1);
            tts.setVoice(voiceNamed(voiceId));
            tts.setSpeechRate(rate);
            Bundle params = new Bundle();
            params.putFloat(TextToSpeech.Engine.KEY_PARAM_VOLUME, volume);
            for (int i = from; i < text.size(); i++) {
                int queued = tts.speak(text.get(i), i == from ? TextToSpeech.QUEUE_FLUSH : TextToSpeech.QUEUE_ADD,
                        params, SpeechSessions.utteranceId(session, i));
                if (queued != TextToSpeech.SUCCESS) {
                    tts.stop();
                    sessions.stop(session);
                    call.reject("this phone's text-to-speech engine refused the reading");
                    return;
                }
            }
            call.resolve();
        });
    }

    @PluginMethod
    public void stop(PluginCall call) {
        String session = call.getString("session", "");
        SpeechSessions.Event stopped = sessions.stop(session);
        if (stopped != null && tts != null) {
            tts.stop();
        }
        send(stopped);
        call.resolve();
    }

    private Voice voiceNamed(String id) {
        Voice fallback = tts.getDefaultVoice();
        if (id == null || id.isEmpty()) {
            return fallback;
        }
        Set<Voice> voices = tts.getVoices();
        if (voices != null) {
            for (Voice voice : voices) {
                if (voice.getName().equals(id)) {
                    return voice;
                }
            }
        }
        return fallback;
    }

    private void send(SpeechSessions.Event event) {
        if (event == null) {
            return;
        }
        JSObject payload = new JSObject();
        payload.put("session", event.session);
        if (event.kind.equals("progress")) {
            payload.put("index", event.index);
        } else {
            payload.put("outcome", event.outcome);
            if (event.message != null) {
                payload.put("message", event.message);
            }
        }
        notifyListeners(event.kind, payload);
    }
}
