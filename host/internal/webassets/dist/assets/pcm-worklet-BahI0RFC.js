// Runs on the audio render thread. No encoded-media or WebRTC input is used.
class StudioLinkPCMProcessor extends AudioWorkletProcessor {
  constructor(options) {
    super();this.channels=0;this.buffer=null;this.position=0;this.totalFrames=0;this.active=true;
    this.maxFrames=options.processorOptions.maxFrames;
    this.port.onmessage=event=>{if(event.data.type==='flush'){this.active=false;this.flush();this.port.postMessage({type:'flushed',frames:this.totalFrames});}};
  }
  flush(){
    if(!this.position||!this.buffer)return;
    const data=this.buffer.slice(0,this.position*this.channels);
    this.port.postMessage({type:'pcm',data:data.buffer,channels:this.channels,sampleRate,frames:this.position,totalFrames:this.totalFrames},[data.buffer]);
    this.position=0;
  }
  fail(message){this.active=false;this.port.postMessage({type:'error',message});}
  process(inputs){
    if(!this.active)return true;
    const input=inputs[0];if(!input?.length||!input[0]?.length)return true;
    if(!this.channels){
      this.channels=input.length;
      if(this.channels>2){this.fail('Only mono or stereo PCM capture is supported');return true;}
      this.buffer=new Float32Array(sampleRate*this.channels);
      this.port.postMessage({type:'format',channels:this.channels,sampleRate,contextFrame:currentFrame});
    }
    if(input.length!==this.channels){this.fail('Microphone channel count changed during PCM capture');return true;}
    const frames=input[0].length;
    // Use the worst-case stereo bound supplied by the recorder, before writing bytes.
    if(this.totalFrames+frames>this.maxFrames){this.fail('PCM recording reached the 32-bit RIFF size limit; committed fragments retained');return true;}
    for(let frame=0;frame<frames;frame++){
      for(let channel=0;channel<this.channels;channel++)this.buffer[this.position*this.channels+channel]=input[channel][frame];
      this.position++;this.totalFrames++;
      if(this.position===sampleRate)this.flush();
    }
    return true;
  }
}
registerProcessor('studiolink-pcm',StudioLinkPCMProcessor);
