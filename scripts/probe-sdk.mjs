import { probeAccount } from './oauth-cli.mjs';
const directory = process.argv[2] || '.state/research/runtime-probe';
const modelIndex = process.argv.indexOf('--model');
console.log(JSON.stringify(await probeAccount(directory, { prompt: process.argv.includes('--prompt'),
  model: modelIndex >= 0 ? process.argv[modelIndex + 1] : undefined }), null, 2));
