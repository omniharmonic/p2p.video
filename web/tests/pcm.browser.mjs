import assert from 'node:assert/strict';
import {fileURLToPath} from 'node:url';
import {createServer} from 'vite';
const {chromium}=await import(process.env.PLAYWRIGHT_MODULE||'playwright');
const server=await createServer({configFile:false,root:fileURLToPath(new URL('../',import.meta.url)),server:{host:'127.0.0.1',port:5192,strictPort:true}});await server.listen();let browser;
try {
 browser=await chromium.launch({headless:true,executablePath:process.env.CHROMIUM_PATH,args:['--autoplay-policy=no-user-gesture-required']});
 const context=await browser.newContext();const page=await context.newPage();await page.goto('http://127.0.0.1:5192');
 const result=await page.evaluate(async()=>{
  const {LocalPcmRecorder}=await import('/src/lib/pcm-recorder.ts');
  const {OPFSStorage}=await import('/src/lib/opfs.ts');const storage=OPFSStorage.getInstance();
  const audio=new AudioContext({sampleRate:48000});await audio.resume();
  const left=audio.createOscillator(),right=audio.createOscillator(),merger=audio.createChannelMerger(2),destination=audio.createMediaStreamDestination();
  left.frequency.value=440;right.frequency.value=880;left.connect(merger,0,0);right.connect(merger,0,1);merger.connect(destination);left.start();right.start();
  const fit={a:0,b:1,rttP10Ms:1};const recorder=new LocalPcmRecorder('pcm',async chunk=>{await storage.saveChunk('test','pcm',chunk.seq,chunk.data);},fit);
  await recorder.start(destination.stream);
  const deadline=performance.now()+10000;while(recorder.getStats().frames<48000&&performance.now()<deadline)await new Promise(resolve=>setTimeout(resolve,30));
  const stopped=await recorder.stop();left.stop();right.stop();destination.stream.getTracks().forEach(track=>track.stop());
  const seqs=await storage.listLocalSeqs('test','pcm'),chunks=[];for(const seq of seqs)chunks.push(await storage.getChunk('test','pcm',seq));
  const blob=new Blob(chunks,{type:'audio/wav'}),buffer=await blob.arrayBuffer();const decoded=await audio.decodeAudioData(buffer.slice(0));
  const frequencies=[];for(let channel=0;channel<decoded.numberOfChannels;channel++){
   const samples=decoded.getChannelData(channel);let crossings=0;const start=Math.round(.1*decoded.sampleRate);for(let i=start+1;i<samples.length;i++)if(samples[i-1]<=0&&samples[i]>0)crossings++;
   frequencies.push(crossings*decoded.sampleRate/(samples.length-start));
  }
  await audio.close();
  const header=new DataView(buffer);return {rate:decoded.sampleRate,channels:decoded.numberOfChannels,duration:decoded.duration,frequencies,bytes:buffer.byteLength,riffBytes:header.getUint32(4,true)+8,dataBytes:header.getUint32(54,true),frames:stopped.sidecar.pcm.frames,captureComplete:stopped.sidecar.captureComplete,seqs,firstEvent:stopped.sidecar.events[0].type,engine:recorder.engine,gapFrames:stopped.sidecar.pcm.gapFrames};
 });
 assert.equal(result.rate,48000);assert.equal(result.channels,2);assert.ok(result.duration>=1&&result.duration<2);
 assert.ok(Math.abs(result.frequencies[0]-440)<3);assert.ok(Math.abs(result.frequencies[1]-880)<3);
 assert.equal(result.bytes,result.riffBytes);assert.equal(result.dataBytes,result.frames*8);assert.equal(result.captureComplete,true);assert.equal(result.seqs[0],0);// Chromium records on the capture clock (MediaStreamTrackProcessor), never the output render clock [SL-QA-2].
 assert.equal(result.engine,'track-processor');assert.match(result.firstEvent,/capture-start-estimate/);assert.equal(result.gapFrames,0);
 console.log(JSON.stringify(result));
}finally{await browser?.close();await server.close();}
