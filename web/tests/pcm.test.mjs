import test from 'node:test';
import assert from 'node:assert/strict';
import {build} from 'esbuild';
import {mkdtemp,rm,readFile} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import vm from 'node:vm';
const dir=await mkdtemp(join(tmpdir(),'studio-pcm-tests-'));
await build({entryPoints:[new URL('../src/lib/wav.ts',import.meta.url).pathname],bundle:true,format:'esm',outfile:join(dir,'wav.mjs')});
const {floatWavHeader,MAX_WAV_BYTES}=await import(join(dir,'wav.mjs'));
test.after(()=>rm(dir,{recursive:true,force:true}));
test('float WAV declares actual channels/rate/frame count and exact RIFF/data lengths',()=>{
 const bytes=floatWavHeader(44100,2,3);const view=new DataView(bytes.buffer);const text=(start,n)=>new TextDecoder().decode(bytes.slice(start,start+n));
 assert.equal(bytes.length,58);assert.equal(text(0,4),'RIFF');assert.equal(view.getUint32(4,true),74);
 assert.equal(text(8,4),'WAVE');assert.equal(view.getUint16(20,true),3);assert.equal(view.getUint16(22,true),2);
 assert.equal(view.getUint32(24,true),44100);assert.equal(view.getUint16(34,true),32);
 assert.equal(text(38,4),'fact');assert.equal(view.getUint32(46,true),3);assert.equal(text(50,4),'data');assert.equal(view.getUint32(54,true),24);
});
test('RIFF overflow and invalid audio geometry are refused before allocating sample buffers',()=>{
 assert.throws(()=>floatWavHeader(48000,2,Math.ceil(MAX_WAV_BYTES/8)),/RIFF|limit/);
 for(const values of [[48000,0,2],[0,1,2],[48000,3,2],[48000,1,-1]])assert.throws(()=>floatWavHeader(...values));
});
test('audio worklet preserves Float32 sample values and interleaves actual stereo channels',async()=>{
 let Processor;const messages=[];
 const sandbox={AudioWorkletProcessor:class {port={onmessage:null,postMessage:message=>messages.push(message)}},sampleRate:4,currentFrame:128,registerProcessor:(_name,processor)=>{Processor=processor;},Float32Array};
 vm.runInNewContext(await readFile(new URL('../src/lib/pcm-worklet.js',import.meta.url),'utf8'),sandbox);
 const processor=new Processor({processorOptions:{maxFrames:16}});
 processor.process([[new Float32Array([.25,-.5]),new Float32Array([-.75,1])]]);
 processor.port.onmessage({data:{type:'flush'}});
 const block=messages.find(message=>message.type==='pcm');
 assert.equal(block.channels,2);assert.equal(block.frames,2);assert.deepEqual([...new Float32Array(block.data)],[.25,-.75,-.5,1]);
 assert.ok(messages.some(message=>message.type==='flushed'));
});

// Capture-clocked PCM [SL-QA-2]. The AudioWorklet engine pulls microphone
// samples at the output device's render rate: under CPU load (the 10-minute
// rehearsal) Chromium's render glitches dropped 1.4-2.6 s of microphone input
// per 10 minutes, and the uncompressed mic fell hundreds of ms behind its own
// camera audio. The track-processor engine records every captured frame on
// the capture clock and fills frames the browser dropped with silence.
await build({entryPoints:[new URL('../src/lib/pcm-recorder.ts',import.meta.url).pathname],bundle:true,format:'esm',outfile:join(dir,'pcm-recorder.mjs')});
const {LocalPcmRecorder,captureGapFrames,CAPTURE_GAP_TOLERANCE_US,MAX_CAPTURE_GAP_FILL_US}=await import(join(dir,'pcm-recorder.mjs'));
test('capture gaps: dropped frames are measured; jitter, overlap and clock jumps are not filled',()=>{
 assert.equal(captureGapFrames(null,1_000_000,48000),0);
 assert.equal(captureGapFrames(1_000_000,1_000_000,48000),0);
 assert.equal(captureGapFrames(1_000_000,1_000_000+CAPTURE_GAP_TOLERANCE_US-1,48000),0,'timestamp jitter');
 assert.equal(captureGapFrames(1_000_000,1_010_600,48000),0,'re-chunked timestamp jitter (measured ±10.6 ms)');
 assert.equal(captureGapFrames(1_000_000,1_020_000,48000),960,'two dropped 10 ms frames');
 assert.equal(captureGapFrames(1_000_000,1_250_000,44100),11025);
 assert.equal(captureGapFrames(1_000_000,990_000,48000),0,'overlap: samples are never removed');
 assert.equal(captureGapFrames(0,MAX_CAPTURE_GAP_FILL_US+1,48000),0,'a clock jump is not audio');
});
function fakeCapture({rate=48000,channels=2,frameMs=10,count,drop=new Set()}){
 const n=rate*frameMs/1000,t0=947_237_624_174;
 const sample=(k,i,c)=>((k*n+i)%1000+1)/1000*(c?-1:1);
 const frames=[];for(let k=0;k<count;k++)if(!drop.has(k))frames.push({timestamp:t0+k*frameMs*1000,numberOfFrames:n,numberOfChannels:channels,sampleRate:rate,
  copyTo(dest,{planeIndex}){for(let i=0;i<n;i++)dest[i]=sample(k,i,planeIndex);},close(){}});
 class Processor{constructor({track,maxBufferSize}){assert.ok(track&&maxBufferSize>=100);this.readable=new ReadableStream({start(c){for(const f of frames)c.enqueue(f);c.close();}});}}
 return {Processor,sample,n};
}
const fakeStream={getAudioTracks:()=>[{getSettings:()=>({deviceId:'mic'}),addEventListener(){}}]};
async function recordCapture(options){
 const {Processor,sample,n}=fakeCapture(options);
 const chunks=[];
 const savedContext=globalThis.AudioContext;
 globalThis.AudioContext=class{constructor(){throw new Error('the render clock must not record when the capture clock is available');}};
 try{
  const recorder=new LocalPcmRecorder('pcm',chunk=>{chunks.push(chunk);},{a:0,b:1,rttP10Ms:1},{trackProcessor:Processor});
  await recorder.start(fakeStream);
  await new Promise(r=>setTimeout(r,20));
  const stopped=await recorder.stop();
  return {recorder,stopped,chunks,sample,n};
 }finally{globalThis.AudioContext=savedContext;}
}
test('track-processor PCM keeps the capture timeline: dropped capture frames become silence of the same length',async()=>{
 const drop=new Set();for(let k=120;k<140;k++)drop.add(k); // 200 ms the browser never delivered
 const {recorder,stopped,chunks,sample,n}=await recordCapture({count:300,drop});
 assert.equal(recorder.engine,'track-processor');
 const pcm=stopped.sidecar.pcm;
 assert.equal(pcm.sampleRate,48000);assert.equal(pcm.channels,2);
 assert.equal(pcm.frames,3*48000,'file duration equals capture time (3 s), not delivered audio (2.8 s)');
 assert.equal(pcm.gapFrames,20*n);
 assert.equal(stopped.sidecar.events[0].type,'pcm-capture-start-estimate');
 assert.ok(stopped.sidecar.events.some(e=>e.type==='pcm-gap-filled'&&e.mediaTimeUs===1_200_000));
 assert.equal(stopped.sidecar.captureComplete,true);
 const body=chunks.filter(c=>c.seq>0).sort((a,b)=>a.seq-b.seq);
 assert.deepEqual(body.map(c=>c.data.byteLength),[48000*8,48000*8,48000*8],'one-second fragments');
 const samples=new Float32Array(body.reduce((total,c)=>total+c.data.byteLength,0)/4);let o=0;
 for(const c of body){samples.set(new Float32Array(c.data.buffer,c.data.byteOffset,c.data.byteLength/4),o);o+=c.data.byteLength/4;}
 const at=(frame,ch)=>samples[frame*2+ch];
 assert.equal(at(0,0),Math.fround(sample(0,0,0)));assert.equal(at(0,1),Math.fround(sample(0,0,1)),'planar channels interleave');
 for(const f of [120*n,125*n,140*n-1])assert.equal(at(f,0),0,'silence where frames were dropped');
 assert.equal(at(140*n,0),Math.fround(sample(140,0,0)),'audio after the gap sits at its capture time');
 assert.equal(at(299*n+5,1),Math.fround(sample(299,5,1)));
 const header=chunks.find(c=>c.seq===0);const view=new DataView(header.data.buffer,header.data.byteOffset);
 assert.equal(view.getUint32(46,true),3*48000);assert.equal(view.getUint32(24,true),48000);
});
test('track-processor PCM records at the capture rate and anchors on the capture clock',async()=>{
 const before=performance.now();
 const {stopped}=await recordCapture({rate:44100,channels:1,count:150});
 const pcm=stopped.sidecar.pcm;
 assert.equal(pcm.sampleRate,44100);assert.equal(pcm.channels,1);assert.equal(pcm.frames,Math.round(1.5*44100));assert.equal(pcm.gapFrames,0);
 // Every fake frame is "captured" before the first is delivered: the anchor
 // is on the performance clock and no later than delivery.
 const anchor=stopped.sidecar.events[0].perfTime;
 assert.ok(Number.isFinite(anchor)&&anchor<=performance.now()&&anchor>before-5000,`anchor ${anchor}`);
});
test('without a track processor the AudioWorklet engine records (Safari, Firefox)',async()=>{
 const recorder=new LocalPcmRecorder('pcm',()=>{},{a:0,b:1,rttP10Ms:1},{trackProcessor:null});
 const saved=globalThis.AudioContext;let constructed=false;
 globalThis.AudioContext=class{constructor(){constructed=true;}close(){return Promise.resolve();}};
 try{await assert.rejects(recorder.start(fakeStream),/AudioWorklet is unavailable/);}finally{globalThis.AudioContext=saved;}
 assert.ok(constructed);assert.equal(recorder.engine,'audio-worklet');
});
