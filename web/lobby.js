import { StoryAudio } from './audio.js';

const message = Object.freeze({
  JOIN: 'lobby.join', LEAVE: 'lobby.leave', END: 'lobby.end', STATE: 'lobby.state',
  NOTICE: 'lobby.notice', ACCEPTED: 'turn.accepted', ERROR: 'error',
  START: 'turn.start', FINISH: 'turn.finish', TEXT: 'turn.text',
  PLAY: 'playback.start', PLAY_END: 'playback.end', PLAY_STOP: 'playback.stop', RESULT: 'playback.result',
});
const result = Object.freeze({ FINISHED: 'finished', STOPPED: 'stopped', FAILED: 'failed' });
const phase = Object.freeze({ WAITING: 'waiting', PLAYING: 'playing', ENDED: 'ended' });
const maxRecordingBytes = 16000 * 2 * 20;
const element = id => document.getElementById(id);
const requestedBox = new URLSearchParams(location.search).get('box')?.toUpperCase();
const boxName = '盒子 ' + (['A', 'B', 'C'].includes(requestedBox) ? requestedBox : 'A');
const target = new URL('/lobby/ws', location.href);
target.protocol = location.protocol === 'https:' ? 'wss:' : 'ws:';
const socket = new WebSocket(target);
socket.binaryType = 'arraybuffer';
let view = {};
let playback = null;
let micRequested = false;
let stoppingMic = false;
let recordedBytes = 0;
let ready = false;

const audio = new StoryAudio({
  onFrame(frame) {
    if (!view.recording || stoppingMic || !ready) return;
    socket.send(frame);
    recordedBytes += frame.byteLength;
    if (recordedBytes >= maxRecordingBytes) void finishRecording();
  },
  onActivity() {},
  onDone() {
    finishPlayback(result.FINISHED);
  },
  onError(text) {
    finishPlayback(result.FAILED);
    showError(text);
  },
});

element('box-title').textContent = boxName;
element('connection').textContent = boxName + ' · 正在连接';

function send(type, data = {}) {
  if (ready) socket.send(JSON.stringify({ type, ...data }));
}

function showError(text = '') {
  const target = element('error');
  target.textContent = text;
  target.hidden = !text;
}

function logEvent(text) {
  const list = element('trace');
  const item = document.createElement('li');
  item.textContent = text;
  list.prepend(item);
  if (list.children.length > 12) list.lastElementChild.remove();
}

function insideRoom() {
  return [phase.WAITING, phase.PLAYING].includes(view.phase);
}

function updateControls() {
  const inside = insideRoom();
  const canText = ready && view.can_speak && !micRequested;
  element('join').disabled = !ready || inside;
  element('topic').disabled = inside;
  element('reuse').disabled = inside;
  element('leave').disabled = !ready || !inside;
  element('end-game').disabled = !ready || !view.can_end;
  element('record').disabled = !ready || micRequested || stoppingMic || !(view.can_speak || view.recording);
  element('record').textContent = view.recording ? '说完了' : '开始说话';
  element('share-audio').disabled = !ready || !(view.can_speak || view.recording);
  element('stop-play').disabled = !ready || !playback;
  element('use-history').disabled = !ready || view.phase !== phase.WAITING || view.members.length !== 1;
  element('sentence').disabled = !canText;
  element('send-text').disabled = !canText;
}

function render(next) {
  view = next;
  if (!view.recording && audio.stream && !micRequested && !stoppingMic) {
    void audio.stopMic().catch(error => showError(error.message));
  }
  if (!insideRoom()) element('share-audio').checked = true;
  element('room-title').textContent = view.topic || '游戏大厅';
  element('room-count').textContent = view.members.length + ' 台盒子';
  let hint = '说「玩故事接龙」，或者点击加入。';
  switch (view.phase) {
    case phase.WAITING:
      hint = view.match_deadline && new Date(view.match_deadline).getFullYear() > 1
        ? '匹配中。三人到齐立即开始，三分钟后两人也能开始。'
        : '还没有伙伴，可以继续等待，或选择和历史伙伴玩。';
      break;
    case phase.PLAYING:
      if (view.waiting_on) hint = '等 ' + view.waiting_on + ' 播完上一句。';
      else if (view.recording) hint = '你在发言，原声会送到这一轮的听众。';
      else if (view.can_speak) hint = '轮到你了。';
      else hint = '轮到 ' + view.speaker + '。';
      break;
    case phase.ENDED:
      hint = '这一局结束了。你可以再开一局。';
      break;
  }
  element('turn-status').textContent = hint;
  const members = view.members.map(member => {
    const li = document.createElement('li');
    li.textContent = member.name + (member.self ? '（你）' : '') + (member.audio ? '' : ' · 仅文字');
    return li;
  });
  element('members').replaceChildren(...members);
  const lines = view.story.map(line => {
    const li = document.createElement('li');
    const author = document.createElement('strong');
    author.textContent = line.author + (line.historical ? ' · 以前留下的' : '');
    li.append(author, '　', line.text || (line.audio ? '原声片段' : ''));
    return li;
  });
  element('lines').replaceChildren(...lines);
  updateControls();
}

function finishPlayback(state) {
  if (!playback) return;
  const id = playback.id;
  playback = null;
  if (state !== result.FINISHED) audio.flush();
  logEvent('播放 ' + id.slice(0, 6) + ' · ' + state);
  send(message.RESULT, { playback_id: id, result: state });
  updateControls();
}

async function join() {
  try {
    await audio.unlock();
    showError();
    send(message.JOIN, {
      name: boxName, topic: element('topic').value, reuse: element('reuse').checked,
    });
  } catch (error) {
    showError('无法开启声音：' + error.message);
  }
}

async function finishRecording() {
  if (stoppingMic) return;
  stoppingMic = true;
  updateControls();
  try {
    await audio.stopMic();
    send(message.FINISH, { share: element('share-audio').checked });
    element('share-audio').checked = true;
  } catch (error) {
    showError('关闭麦克风失败：' + error.message);
  } finally {
    stoppingMic = false;
    updateControls();
  }
}

async function beginRecording() {
  micRequested = true;
  updateControls();
  try {
    recordedBytes = 0;
    await audio.startMic();
    if (view.can_speak && ready) send(message.START);
    else await audio.stopMic();
  } catch (error) {
    showError('无法开启麦克风：' + error.message);
  } finally {
    micRequested = false;
    updateControls();
  }
}

socket.addEventListener('open', () => {
  ready = true;
  element('connection').textContent = boxName + ' · 本地模拟';
  updateControls();
});
socket.addEventListener('close', () => {
  ready = false;
  element('connection').textContent = boxName + ' · 已断开';
  element('turn-status').textContent = '与大厅断开，请刷新页面。';
  void audio.close().catch(error => showError(error.message));
  updateControls();
});
socket.addEventListener('message', ({ data }) => {
  if (data instanceof ArrayBuffer) {
    if (playback) {
      try {
        audio.push(data);
      } catch (error) {
        finishPlayback(result.FAILED);
        showError(error.message);
      }
    }
    return;
  }
  const event = JSON.parse(data);
  switch (event.type) {
    case message.STATE:
      render(event.state);
      break;
    case message.PLAY:
      if (playback) finishPlayback(result.STOPPED);
      audio.flush();
      playback = event.play;
      if (playback.historical) {
        element('notice').textContent = '这是 ' + playback.author + ' 以前留下的原声。';
      }
      logEvent('开始播放 · ' + playback.author + (playback.historical ? '（历史原声）' : ''));
      updateControls();
      break;
    case message.PLAY_END:
      if (playback?.id === event.play.id) {
        audio.finish().catch(error => {
          if (playback?.id === event.play.id) finishPlayback(result.FAILED);
          showError(error.message);
        });
      }
      break;
    case message.PLAY_STOP:
      if (playback?.id === event.play.id) finishPlayback(result.STOPPED);
      break;
    case message.ACCEPTED:
    case message.NOTICE:
      element('notice').textContent = event.text;
      showError();
      break;
    case message.ERROR:
      if (!view.recording && audio.stream) void audio.stopMic().catch(error => showError(error.message));
      showError(event.text);
      break;
  }
});

element('join-form').addEventListener('submit', event => {
  event.preventDefault();
  void join();
});
element('text-form').addEventListener('submit', event => {
  event.preventDefault();
  const input = element('sentence');
  const text = input.value.trim();
  if (!text || !view.can_speak) return;
  showError();
  send(message.TEXT, { text });
  input.value = '';
});
element('record').addEventListener('click', () => {
  showError();
  if (view.recording) void finishRecording();
  else void beginRecording();
});
element('stop-play').addEventListener('click', () => {
  if (!playback) return;
  send(message.PLAY_STOP, { playback_id: playback.id });
  finishPlayback(result.STOPPED);
});
element('use-history').addEventListener('click', () => send('lobby.reuse'));
element('leave').addEventListener('click', () => {
  audio.flush();
  playback = null;
  send(message.LEAVE);
});
element('end-game').addEventListener('click', () => send(message.END));

const SpeechRecognition = window.SpeechRecognition || window.webkitSpeechRecognition;
if (SpeechRecognition) {
  const button = element('wake');
  button.hidden = false;
  button.addEventListener('click', async () => {
    try {
      await audio.unlock();
      const recognition = new SpeechRecognition();
      recognition.lang = 'zh-CN';
      recognition.onresult = ({ results }) => {
        const phrase = results[0][0].transcript.trim();
        if (!insideRoom() && /故事接龙|游戏大厅/.test(phrase)) void join();
        else showError('请说「玩故事接龙」。');
      };
      recognition.onerror = () => showError('浏览器语音识别不可用，可以点击「加入故事接龙」。');
      recognition.onend = () => { button.disabled = false; };
      button.disabled = true;
      showError();
      element('notice').textContent = '请说「玩故事接龙」。';
      recognition.start();
    } catch (error) {
      button.disabled = false;
      showError(error.message);
    }
  });
}
