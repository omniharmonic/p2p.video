import type {ClockFit} from './clock';
import type {ChunkItem,SidecarData} from './recorder';
import type {RecordingMicState} from './protocol';
import {floatWavHeader,MAX_WAV_BYTES,WAV_HEADER_BYTES} from './wav';

/** The parts of a WebCodecs AudioData the capture engine reads. */
export interface CapturedAudio {
  /** Capture time of the first frame, in microseconds. */
  readonly timestamp:number;
  readonly numberOfFrames:number;
  readonly numberOfChannels:number;
  readonly sampleRate:number;
  copyTo(destination:Float32Array,options:{planeIndex:number;format?:'f32-planar'}):void;
  close():void;
}
/** MediaStreamTrackProcessor for audio (Chromium; not yet in TypeScript's DOM lib). */
export type TrackProcessorConstructor=new(init:{track:MediaStreamTrack;maxBufferSize?:number})=>{readable:ReadableStream<CapturedAudio>};

export interface PcmRecorderOptions {
  sampleRate?: number;
  replacesAudioOfTrackId?: string;
  /** Recording mic mode and measured processing [SL-P1-10]. */
  micProcessing?: RecordingMicState;
  onError?: (error:Error)=>void;
  /**
   * Capture-clocked engine (MediaStreamTrackProcessor). Defaults to the
   * browser's; null forces the AudioWorklet engine. Injected by tests.
   */
  trackProcessor?: TrackProcessorConstructor|null;
}

/**
 * A gap between consecutive capture frames at least this long is dropped
 * audio, not timestamp jitter. Chromium delivers 10 ms frames; a microphone's
 * timestamps were measured continuous to the microsecond, but a re-chunked
 * WebAudio track's jitter by up to ±10.6 ms (summing to zero). 1.5 frames
 * separates the two, and a slowly drifting device clock never jumps this far.
 */
export const CAPTURE_GAP_TOLERANCE_US=15000;
/** A longer jump is a clock discontinuity (device change, sleep), not dropped audio: never filled. */
export const MAX_CAPTURE_GAP_FILL_US=10_000_000;
/** Frames the track processor may queue while the page is busy (10 ms each: one minute). */
export const TRACK_PROCESSOR_BUFFER_FRAMES=6000;

/**
 * Pure [SL-QA-2]: frames of silence that keep the file's timeline equal to
 * capture time when the frame starting at `timestampUs` follows one that
 * ended at `previousEndUs` (capture frames were dropped before reaching the
 * recorder). Jitter, overlaps and clock jumps return 0: samples are never
 * removed and slow device-clock drift is never "corrected" here (the host
 * measures drift against the take).
 */
export function captureGapFrames(previousEndUs:number|null,timestampUs:number,sampleRate:number):number {
  if(previousEndUs===null||!Number.isFinite(previousEndUs)||!Number.isFinite(timestampUs))return 0;
  const gapUs=timestampUs-previousEndUs;
  if(gapUs<CAPTURE_GAP_TOLERANCE_US||gapUs>MAX_CAPTURE_GAP_FILL_US)return 0;
  return Math.round(gapUs*sampleRate/1e6);
}

// Uncompressed samples from a dedicated local microphone stream. Web Audio/device
// processing can precede these samples; this is not a physical ADC fidelity claim.
//
// Two engines [SL-QA-2]:
//   track-processor  MediaStreamTrackProcessor (Chromium): every captured frame,
//                    with its capture timestamp, at the device's own rate. Frames
//                    the browser dropped before delivery become silence of the
//                    same length, so the file stays on the capture timeline.
//   audio-worklet    an AudioContext render (Safari, Firefox). Its samples are
//                    pulled at the *output* device's render rate: when rendering
//                    glitches under CPU load, Chromium measured 1.4-2.6 s of
//                    microphone input lost per 10 minutes, and the file falls
//                    behind its camera. Kept only where the processor is missing.
export class LocalPcmRecorder {
  private context:AudioContext|null=null;
  private source:MediaStreamAudioSourceNode|null=null;
  private node:AudioWorkletNode|null=null;
  private silentOutput:GainNode|null=null;
  private reader:ReadableStreamDefaultReader<CapturedAudio>|null=null;
  private readLoop:Promise<void>|null=null;
  private started=false;
  private pending=Promise.resolve();
  private pendingBytes=0;
  private failure:Error|null=null;
  private stopping=false;
  private nextSeq=1;
  private flushed:(()=>void)|null=null;
  private formatReady:(()=>void)|null=null;
  private formatFailed:((reason:Error)=>void)|null=null;
  private stopPromise:Promise<{lastSeq:number;sidecar:SidecarData}>|null=null;
  private sidecar:SidecarData;
  // Track-processor engine state.
  private block:Float32Array|null=null;
  private blockFrames=0;
  private totalFrames=0;
  private maxFrames=Math.floor((MAX_WAV_BYTES-WAV_HEADER_BYTES)/8);
  private previousEndUs:number|null=null;
  private firstTimestampUs=0;
  private perfMinusCaptureMs=Infinity;
  private anchorFinal=false;
  private planes:Float32Array[]=[];

  constructor(public readonly trackId:string,private onChunk:(chunk:ChunkItem)=>Promise<void>|void,
    clockFit:ClockFit,private options:PcmRecorderOptions={}) {
    this.sidecar={trackId,kind:'audio',engine:'audio-worklet',codec:'pcm_f32le',mimeType:'audio/wav',clockFit,events:[],captureComplete:false,replacesAudioOfTrackId:options.replacesAudioOfTrackId,...(options.micProcessing?{micProcessing:options.micProcessing}:{})};
  }
  public getStats():{sampleRate:number;channels:number;frames:number} {
    return {sampleRate:this.sidecar.pcm?.sampleRate||0,channels:this.sidecar.pcm?.channels||0,frames:this.sidecar.pcm?.frames||0};
  }
  /** Which engine records ('track-processor' or 'audio-worklet'). */
  public get engine():string { return this.sidecar.engine||''; }
  private fail(reason:unknown):void {
    if(this.failure)return;
    this.failure=reason instanceof Error?reason:new Error(String(reason));
    this.formatFailed?.(this.failure);
    this.source?.disconnect();this.node?.disconnect();this.silentOutput?.disconnect();
    if(this.context&&this.context.state!=='closed')void this.context.close().catch(()=>{});
    void this.reader?.cancel().catch(()=>{});
    this.options.onError?.(this.failure);
  }
  /** Journals one block of interleaved samples (totalFrames: frames in the file after it). */
  private emitBlock(bytes:Uint8Array,totalFrames:number):void {
    const pcm=this.sidecar.pcm!;
    if(this.pendingBytes+bytes.byteLength>64*1024*1024){this.fail(new Error('PCM write buffer overflow or invalid sample block'));return;}
    pcm.frames=totalFrames;
    try {floatWavHeader(pcm.sampleRate,pcm.channels,pcm.frames);}catch(error){this.fail(error);return;}
    this.anchorFinal=true;
    const sidecar=structuredClone(this.sidecar),seq=this.nextSeq++,perfTimeMs=performance.now();
    this.pendingBytes+=bytes.byteLength;
    this.pending=this.pending.then(async()=>{if(!this.failure)await this.onChunk({seq,data:bytes,perfTimeMs,sidecar});}).catch(error=>this.fail(error)).finally(()=>{this.pendingBytes-=bytes.byteLength;});
  }
  public async start(stream:MediaStream):Promise<void> {
    if(this.started)throw new Error('PCM recorder cannot be reused');
    this.started=true;
    if(!stream.getAudioTracks().length)throw new Error('No local microphone for PCM capture');
    const Processor=this.options.trackProcessor===undefined?(globalThis as {MediaStreamTrackProcessor?:TrackProcessorConstructor}).MediaStreamTrackProcessor:this.options.trackProcessor;
    if(typeof Processor==='function'){
      let readable:ReadableStream<CapturedAudio>|null=null;
      try {readable=new Processor({track:stream.getAudioTracks()[0],maxBufferSize:TRACK_PROCESSOR_BUFFER_FRAMES}).readable;}catch{/* No audio support: the worklet records. */}
      if(readable){await this.startTrackProcessor(stream,readable);return;}
    }
    await this.startWorklet(stream);
  }

  // ---- track-processor engine (capture clock).
  private async startTrackProcessor(stream:MediaStream,readable:ReadableStream<CapturedAudio>):Promise<void> {
    this.sidecar.engine='track-processor';
    const ready=new Promise<void>((resolve,reject)=>{this.formatReady=resolve;this.formatFailed=reject;});
    const timer=setTimeout(()=>this.fail(new Error('PCM microphone did not produce samples within 5 seconds')),5000);
    for(const track of stream.getAudioTracks())track.addEventListener('ended',()=>{if(!this.stopping)this.fail(new Error('PCM microphone disconnected'));});
    const reader=readable.getReader();this.reader=reader;
    this.readLoop=(async()=>{
      try {
        for(;;){
          const {value,done}=await reader.read();
          if(done)break;
          try {if(!this.failure&&!this.stopping)this.onCaptured(value,stream);}finally{value.close();}
        }
      }catch(error){if(!this.stopping)this.fail(error);}
    })();
    try {await ready;}catch(error){this.fail(error);throw error;}finally{clearTimeout(timer);}
    if(this.failure)throw this.failure;
  }
  private onCaptured(frame:CapturedAudio,stream:MediaStream):void {
    const rate=frame.sampleRate,channels=frame.numberOfChannels,frames=frame.numberOfFrames;
    const received=performance.now();
    if(!this.sidecar.pcm){
      if(channels>2){this.fail(new Error('Only mono or stereo PCM capture is supported'));return;}
      try {floatWavHeader(rate,channels,0);}catch(error){this.fail(error);return;}
      this.sidecar.pcm={format:'float32le',sampleRate:rate,channels,frames:0,gapFrames:0,sourceSettings:stream.getAudioTracks()[0].getSettings(),timing:'capture timestamps (MediaStreamTrackProcessor); capture frames dropped before delivery are filled with silence'};
      this.sidecar.bitrate=rate*channels*32;
      this.block=new Float32Array(rate*channels);
      this.firstTimestampUs=frame.timestamp;
      this.sidecar.events.push({type:'pcm-capture-start-estimate',perfTime:received,mediaTimeUs:0});
      this.formatReady?.();
    }
    const pcm=this.sidecar.pcm;
    if(channels!==pcm.channels||rate!==pcm.sampleRate){this.fail(new Error('PCM format changed during capture'));return;}
    // Capture time of the first sample on the performance clock: the frame
    // can't arrive before it was captured, so the smallest (arrival - capture)
    // seen before the first block is journaled bounds it most tightly.
    if(!this.anchorFinal){
      const endUs=frame.timestamp+frames/rate*1e6;
      this.perfMinusCaptureMs=Math.min(this.perfMinusCaptureMs,received-endUs/1000);
      this.sidecar.events[0].perfTime=this.firstTimestampUs/1000+this.perfMinusCaptureMs;
    }
    const gap=captureGapFrames(this.previousEndUs,frame.timestamp,rate);
    if(gap>0){
      pcm.gapFrames=(pcm.gapFrames||0)+gap;
      if(this.sidecar.events.length<1000)this.sidecar.events.push({type:'pcm-gap-filled',perfTime:received,mediaTimeUs:Math.round(this.totalFrames/rate*1e6)});
      this.append(null,gap);
    }
    this.previousEndUs=frame.timestamp+frames/rate*1e6;
    if(this.planes.length!==channels||this.planes[0].length<frames)this.planes=Array.from({length:channels},()=>new Float32Array(frames));
    for(let c=0;c<channels;c++)frame.copyTo(this.planes[c],{planeIndex:c,format:'f32-planar'});
    this.append(this.planes,frames);
  }
  /** Appends `frames` frames of planar samples (null: silence), journaling every full second. */
  private append(planes:Float32Array[]|null,frames:number):void {
    const pcm=this.sidecar.pcm!,channels=pcm.channels,block=this.block!,blockLength=pcm.sampleRate;
    if(this.totalFrames+frames>this.maxFrames){this.fail(new Error('PCM recording reached the 32-bit RIFF size limit; committed fragments retained'));return;}
    let done=0;
    while(done<frames&&!this.failure){
      const n=Math.min(frames-done,blockLength-this.blockFrames);
      for(let i=0;i<n;i++)for(let c=0;c<channels;c++)block[(this.blockFrames+i)*channels+c]=planes?planes[c][done+i]:0;
      this.blockFrames+=n;this.totalFrames+=n;done+=n;
      if(this.blockFrames===blockLength)this.flushBlock();
    }
  }
  private flushBlock():void {
    if(!this.blockFrames||!this.block)return;
    const data=this.block.slice(0,this.blockFrames*this.sidecar.pcm!.channels);
    this.blockFrames=0;
    this.emitBlock(new Uint8Array(data.buffer),this.totalFrames);
  }

  // ---- audio-worklet engine (output render clock; browsers without the processor).
  private async startWorklet(stream:MediaStream):Promise<void> {
    this.context=new AudioContext({sampleRate:this.options.sampleRate||48000,latencyHint:'interactive'});
    const context=this.context;
    if(!context.audioWorklet){await context.close();throw new Error('AudioWorklet is unavailable');}
    try {
      await context.audioWorklet.addModule(new URL('./pcm-worklet.js',import.meta.url).href);
      this.node=new AudioWorkletNode(context,'studiolink-pcm',{numberOfInputs:1,numberOfOutputs:1,outputChannelCount:[1],processorOptions:{maxFrames:this.maxFrames}});
      this.source=context.createMediaStreamSource(stream);
      this.silentOutput=context.createGain();this.silentOutput.gain.value=0;
      const ready=new Promise<void>((resolve,reject)=>{this.formatReady=resolve;this.formatFailed=reject;});
      const timer=setTimeout(()=>this.fail(new Error('PCM microphone did not produce samples within 5 seconds')),5000);
      this.node.onprocessorerror=()=>this.fail(new Error('PCM audio worklet failed'));
      this.node.port.onmessage=(event:MessageEvent)=>{
        const message=event.data;
        if(message.type==='error'){this.fail(new Error(message.message));return;}
        if(this.failure)return;
        if(message.type==='format'){
          const rate=Number(message.sampleRate),channels=Number(message.channels);
          try {floatWavHeader(rate,channels,0);}catch(error){this.fail(error);return;}
          this.sidecar.pcm={format:'float32le',sampleRate:rate,channels,frames:0,sourceSettings:stream.getAudioTracks()[0].getSettings(),timing:'AudioContext render start estimate; input capture latency is not calibrated'};
          this.sidecar.bitrate=rate*channels*32;
          // Anchor only after the worklet has observed a real render block.
          const estimate=performance.now()-(context.currentTime-message.contextFrame/rate)*1000;
          this.sidecar.events.push({type:'pcm-render-start-estimate',perfTime:estimate,mediaTimeUs:0});
          clearTimeout(timer);this.formatReady?.();
        }else if(message.type==='pcm'){
          const pcm=this.sidecar.pcm;
          if(!pcm||message.channels!==pcm.channels||message.sampleRate!==pcm.sampleRate){this.fail(new Error('PCM format changed during capture'));return;}
          const bytes=new Uint8Array(message.data as ArrayBuffer);
          if(bytes.byteLength!==message.frames*pcm.channels*4){this.fail(new Error('PCM write buffer overflow or invalid sample block'));return;}
          this.emitBlock(bytes,message.totalFrames);
        }else if(message.type==='flushed')this.flushed?.();
      };
      for(const track of stream.getAudioTracks())track.addEventListener('ended',()=>{if(!this.stopping)this.fail(new Error('PCM microphone disconnected'));});
      this.source.connect(this.node);this.node.connect(this.silentOutput);this.silentOutput.connect(context.destination);
      try {await Promise.all([ready,context.resume()]);}finally {clearTimeout(timer);}
      if(this.failure)throw this.failure;
    }catch(error){this.fail(error);throw error;}
  }
  public stop():Promise<{lastSeq:number;sidecar:SidecarData}> {
    if(this.stopPromise)return this.stopPromise;
    this.stopping=true;
    this.stopPromise=(async()=>{
      try {
        if(this.failure)throw this.failure;
        if(this.reader){
          await this.reader.cancel().catch(()=>{});
          await this.readLoop;
          if(this.failure)throw this.failure;
          this.flushBlock();
        }else{
          if(!this.node||!this.context)throw new Error('PCM recorder has not started');
          await new Promise<void>((resolve,reject)=>{
            const timer=setTimeout(()=>reject(new Error('PCM final flush timed out')),5000);
            this.flushed=()=>{clearTimeout(timer);resolve();};
            this.node!.port.postMessage({type:'flush'});
          });
        }
        await this.pending;
        if(this.failure)throw this.failure;
        const pcm=this.sidecar.pcm;
        if(!pcm?.frames)throw new Error('PCM microphone produced no samples');
        this.sidecar.captureComplete=true;
        await this.onChunk({seq:0,data:floatWavHeader(pcm.sampleRate,pcm.channels,pcm.frames),perfTimeMs:performance.now(),sidecar:structuredClone(this.sidecar)});
        this.sidecar.events.push({type:'pcm-stop',perfTime:performance.now(),mediaTimeUs:Math.round(pcm.frames/pcm.sampleRate*1e6)});
        return {lastSeq:this.nextSeq-1,sidecar:this.sidecar};
      }catch(error){this.fail(error);throw error;}
      finally {this.source?.disconnect();this.node?.disconnect();this.silentOutput?.disconnect();if(this.context&&this.context.state!=='closed')await this.context.close();}
    })();
    return this.stopPromise;
  }
}
