<template>
  <div>
    <div>
      <input
        v-model="text"
        placeholder="输入文本，回车发送"
        @keyup.enter="sendText"
        style="width: 300px; padding: 8px"
      />
      <button @click="sendText">发送</button>
    </div>

    <div style="margin-top: 20px;">
      <div v-for="(msg, index) in showMessages" :key="index">
        <b>{{ msg.role }}:</b> {{ msg.content }}
        <button v-if="msg.audioUrl" @click="playAudio(msg.audioUrl)">播放</button>
      </div>
    </div>
  </div>
</template>

<script>
import Vue from 'vue';

export default {
  data() {
    return {
      text: '',
      //message: '',
      chunk: {id:'', content:''},
      messages: [],
      sessionID: 'sess1',
      wsText: null,
      wsAudio: null,
      audioCtx: null,
      audioPlayingNodes: [],
      userInteracted: false,
      audioQueueTime: 0, // 新增：下一个音频块播放的起始时间
    };
  },
  computed: {
    showMessages() {
      const all = [...this.messages];
      if (this.chunk.content.trim()) {
        all.push({ role: 'assistant', content: this.chunk.content, id: this.chunk.id });
      }
      return all;
    }
  },
  methods: {
    sendText() {
      if (!this.text.trim()) return;

      if (!this.userInteracted) {
        this.resumeAudioContext();
        this.userInteracted = true;
      }

      this.wsText.send(this.text.trim());
      this.messages.push({ role: 'user', content: this.text.trim() });
      this.text = '';
    },
    resumeAudioContext() {
      if (this.audioCtx && this.audioCtx.state === 'suspended') {
        this.audioCtx.resume();
      }
    },
    setupTextSocket() {
      this.wsText = new WebSocket(`ws://localhost:8080/ws/text-stream?sessionid=${this.sessionID}`);

      this.wsText.onopen = () => {
        console.log('文本 WebSocket 已连接');
      };

      this.wsText.onmessage = (event) => {
        const chunk = JSON.parse(event.data);
        this.chunk.content += chunk.content;

        if (chunk.content === '\n\n') {
          this.messages.push({ role: 'assistant', content: this.chunk.content, id: chunk.id });
          this.chunk = { id: '', content: '' };
        }
      };

      this.wsText.onerror = (e) => {
        console.error('文本 WebSocket 错误', e);
      };

      this.wsText.onclose = () => {
        console.log('文本 WebSocket 已关闭');
      };
    },
    setupAudioSocket() {
      this.audioCtx = new (window.AudioContext || window.webkitAudioContext)();
      this.audioQueueTime = this.audioCtx.currentTime;

      this.wsAudio = new WebSocket(`ws://localhost:8080/ws/audio-stream?sessionid=${this.sessionID}`);
      this.wsAudio.binaryType = 'arraybuffer';

      this.wsAudio.onopen = () => {
        console.log('音频 WebSocket 已连接');
      };

      this.wsAudio.onmessage = (event) => {
        if (typeof event.data === "string") {
          const resp = JSON.parse(event.data)

          console.log("messageID=", resp.messageID, "audioUrl=", resp.audioUrl);
          const message = this.messages.find(msg => msg.id === resp.messageID);
          if (message) {
            //message.audioUrl = resp.audioUrl;
            Vue.set(message, 'audioUrl', resp.audioUrl);
          }
          return
        }

        if (!this.userInteracted) {
          this.resumeAudioContext();
          this.userInteracted = true;
        }

        try {
          const arrayBuffer = event.data;
          console.log('接收到音频数据，字节长度:', arrayBuffer.byteLength);

          // 后端是16位PCM，采样率24000Hz
          // 注意用DataView逐个读取确保小端
          const dataView = new DataView(arrayBuffer);
          const length = arrayBuffer.byteLength / 2;
          const float32Data = new Float32Array(length);
          for (let i = 0; i < length; i++) {
            const int16 = dataView.getInt16(i * 2, true); // true = little endian
            float32Data[i] = int16 / 32768;
          }

          const sampleRate = 24000;

          const audioBuffer = this.audioCtx.createBuffer(1, float32Data.length, sampleRate);
          audioBuffer.getChannelData(0).set(float32Data);

          const source = this.audioCtx.createBufferSource();
          source.buffer = audioBuffer;
          source.connect(this.audioCtx.destination);

          // 按队列时间安排播放，避免重叠和跳过
          const now = this.audioCtx.currentTime;
          const startAt = Math.max(this.audioQueueTime, now);
          source.start(startAt);
          this.audioQueueTime = startAt + audioBuffer.duration;

          source.onended = () => {
            const idx = this.audioPlayingNodes.indexOf(source);
            if (idx !== -1) this.audioPlayingNodes.splice(idx, 1);
          };

          this.audioPlayingNodes.push(source);

          console.log(`播放音频块: 样本数=${float32Data.length}, 播放时长=${audioBuffer.duration.toFixed(3)}秒, 计划开始时间=${startAt.toFixed(3)}`);

        } catch (err) {
          console.error('播放音频数据失败:', err);
        }
      };

      this.wsAudio.onerror = (e) => {
        console.error('音频 WebSocket 错误', e);
      };

      this.wsAudio.onclose = () => {
        console.log('音频 WebSocket 已关闭');
        this.stopAllAudio();
      };
    },
    stopAllAudio() {
      this.audioPlayingNodes.forEach(node => {
        try {
          node.stop();
        } catch (e) {
          console.error('停止音频节点失败:', e);
        }
      });
      this.audioPlayingNodes = [];
      this.audioQueueTime = this.audioCtx ? this.audioCtx.currentTime : 0;
    },
    playAudio(audioUrl) {
      const audio = new Audio(audioUrl);
      audio.play().catch(err => {
        console.log("播放音频时出错:", err);
      });
    }
  },
  mounted() {
    window.addEventListener('click', () => {
      if (!this.userInteracted) {
        this.resumeAudioContext();
        this.userInteracted = true;
      }
    }, { once: true });

    this.setupTextSocket();
    this.setupAudioSocket();
  },
  beforeDestroy() {
    if (this.wsText) this.wsText.close();
    if (this.wsAudio) this.wsAudio.close();
    this.stopAllAudio();
    if (this.audioCtx) this.audioCtx.close();
  }
};
</script>

<style scoped>
input {
  margin-right: 8px;
}
</style>
