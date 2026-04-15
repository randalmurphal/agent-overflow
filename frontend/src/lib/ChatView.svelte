<script lang="ts">
  import MessageList from "./MessageList.svelte";
  import Composer from "./Composer.svelte";

  interface Message {
    id: string;
    role: "user" | "assistant";
    content: string;
    timestamp: number;
  }

  let messages: Message[] = $state([]);

  function handleSend(content: string) {
    const userMsg: Message = {
      id: crypto.randomUUID(),
      role: "user",
      content,
      timestamp: Date.now(),
    };
    messages.push(userMsg);
  }
</script>

<div class="flex flex-col flex-1 min-w-0">
  <MessageList {messages} />
  <Composer onSend={handleSend} />
</div>
