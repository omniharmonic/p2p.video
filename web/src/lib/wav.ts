// IEEE Float32 WAVE preserves the Web Audio sample representation without quantization.
// A RIFF file uses 32-bit sizes. Stop before this limit; never wrap lengths.
export const WAV_HEADER_BYTES = 58;
export const MAX_WAV_BYTES = 0xffffffff + 8;
export function floatWavHeader(sampleRate:number,channels:number,frames:number):Uint8Array {
  if(!Number.isInteger(sampleRate)||sampleRate<8000||sampleRate>192000||!Number.isInteger(channels)||channels<1||channels>2||!Number.isSafeInteger(frames)||frames<0)throw new Error('Invalid PCM audio geometry');
  const dataBytes=frames*channels*4;
  if(dataBytes+WAV_HEADER_BYTES>MAX_WAV_BYTES)throw new Error('PCM recording reached the 32-bit RIFF size limit');
  const bytes=new Uint8Array(WAV_HEADER_BYTES);const view=new DataView(bytes.buffer);
  const text=(offset:number,value:string)=>{for(let i=0;i<value.length;i++)bytes[offset+i]=value.charCodeAt(i);};
  text(0,'RIFF');view.setUint32(4,dataBytes+WAV_HEADER_BYTES-8,true);text(8,'WAVE');text(12,'fmt ');
  view.setUint32(16,18,true);view.setUint16(20,3,true);view.setUint16(22,channels,true);
  view.setUint32(24,sampleRate,true);view.setUint32(28,sampleRate*channels*4,true);view.setUint16(32,channels*4,true);view.setUint16(34,32,true);view.setUint16(36,0,true);
  text(38,'fact');view.setUint32(42,4,true);view.setUint32(46,frames,true);text(50,'data');view.setUint32(54,dataBytes,true);
  return bytes;
}
