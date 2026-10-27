// Synthetic media only. Set PLAYWRIGHT_MODULE to an installed Playwright module
// when it is not in this project's dependencies. Uses a temporary browser profile.
import assert from 'node:assert/strict';
import { fileURLToPath } from 'node:url';
import { createServer } from 'vite';
const { chromium } = await import(process.env.PLAYWRIGHT_MODULE || 'playwright');
let receivedBytes=0;const receivedSeqs=new Set();
const uploadPlugin={name:'synthetic-upload',configureServer(server){server.middlewares.use((req,res,next)=>{
  if(!req.url.startsWith('/api/v1/takes/')) return next();
  if(req.headers.authorization!=='Bearer synthetic-token'){res.statusCode=401;res.end('Wrong grant');return;}
  let size=0;req.on('data',bytes=>{size+=bytes.length});req.on('end',()=>{
    res.setHeader('Content-Type','application/json');
    if(req.url.endsWith('/status'))res.end(JSON.stringify({have:receivedSeqs.size?[[0,receivedSeqs.size-1]]:[]}));
    else if(req.url.endsWith('/complete'))res.end(JSON.stringify({status:'assembled',totalBytes:receivedBytes,sha256:'a'.repeat(64)}));
    else {const seq=Number(req.url.split('/').at(-1));if(!receivedSeqs.has(seq)){receivedSeqs.add(seq);receivedBytes+=size;}res.end('{}');}
  });
});}};
const server = await createServer({ configFile:false,plugins:[uploadPlugin], root:fileURLToPath(new URL('../',import.meta.url)), server:{host:'127.0.0.1',port:0} });
await server.listen();
let browser;
try {
browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_PATH,
  args: ['--autoplay-policy=no-user-gesture-required'] });
  const context = await browser.newContext();
  const page = await context.newPage();
  await page.goto(`http://127.0.0.1:${server.httpServer.address().port}`);
  const result = await page.evaluate(async () => {
    const { LocalTrackRecorder } = await import('/src/lib/recorder.ts');
    const { ProgressiveUploadQueue, recoverTakeFromOPFS } = await import('/src/lib/upload.ts');
    const { OPFSStorage } = await import('/src/lib/opfs.ts');
    let transferError;
    const queue = new ProgressiveUploadQueue('synthetic-take','synthetic','synthetic-token',progress=>{transferError=progress.error}, {sleep:async()=>{}});
    await queue.ready();
    const canvas = document.createElement('canvas'); canvas.width = 160; canvas.height = 90;
    const ctx = canvas.getContext('2d');
    const paint = setInterval(() => { ctx.fillStyle = `rgb(${Date.now()%255},20,30)`; ctx.fillRect(0,0,160,90); },30);
    const audio = new AudioContext(); await audio.resume();
    const oscillator = audio.createOscillator(); const destination = audio.createMediaStreamDestination();
    oscillator.connect(destination); oscillator.start();
    const stream = canvas.captureStream(10); stream.addTrack(destination.stream.getAudioTracks()[0]);
    const chunks = [];
    const recorder = new LocalTrackRecorder('synthetic','video', async chunk => { chunks.push(chunk.data); await queue.enqueue(chunk); }, {a:0,b:1,rttP10Ms:0});
    await recorder.start(stream);
    const audioEnd = audio.currentTime + 1.8;
    const deadline = performance.now() + 10_000;
    while (audio.currentTime < audioEnd && performance.now() < deadline) await new Promise(resolve => setTimeout(resolve,50));
    const stopped = await recorder.stop();
    const complete = await queue.complete(stopped.lastSeq,stopped.sidecar);
    const meta = await OPFSStorage.getInstance().getMeta('synthetic-take','synthetic');
    meta.isComplete=false; await OPFSStorage.getInstance().saveMeta('synthetic-take','synthetic',meta);
    const recovered=await recoverTakeFromOPFS(meta,'unrelated-host-token');
    const localSeqs = await OPFSStorage.getInstance().listLocalSeqs('synthetic-take','synthetic');
    clearInterval(paint); oscillator.stop(); stream.getTracks().forEach(track => track.stop());
    const blob = new Blob(chunks,{type:recorder.getMimeType()});
    const decoded = await audio.decodeAudioData(await blob.arrayBuffer());
    let energy = 0; for (const sample of decoded.getChannelData(0)) energy += sample*sample;
    const video = document.createElement('video'); video.muted=true; video.src=URL.createObjectURL(blob);
    await new Promise((resolve,reject) => { video.onloadeddata=resolve; video.onerror=()=>reject(Error('Video decode failed')); });
    await audio.close();
    return {mime:recorder.getMimeType(), bytes:blob.size, lastSeq:stopped.lastSeq, complete,
      transferError, recovered, durableComplete:(await OPFSStorage.getInstance().getMeta('synthetic-take','synthetic')).isComplete, durableChunks:localSeqs.length, receiptBytes:meta.receipt?.totalBytes,
      videoWidth:video.videoWidth, videoHeight:video.videoHeight, audioDuration:decoded.duration, audioEnergy:energy};
  });
  console.log(JSON.stringify(result));
  assert.equal(result.recovered,true);
  assert.equal(result.complete,true); assert.equal(result.durableComplete,true);
  assert.equal(result.receiptBytes,result.bytes); assert.equal(result.durableChunks,result.lastSeq+1);
  assert.equal(result.videoWidth,160); assert.equal(result.videoHeight,90);
  assert.ok(result.audioDuration >= 1.4); assert.ok(result.audioEnergy > 1);
  assert.ok(result.bytes > 1000); assert.ok(result.lastSeq >= 1);
} finally { await browser?.close(); await server.close(); }
