import { mount } from 'svelte';
import App from './App.svelte';
import './style.css';

// Step 22 entry: mounts the shell only. Hash routes and screens land
// in Steps 23-24; the entry shape stays stable so they slot in here.
const el = document.getElementById('app');
if (el) {
  mount(App, { target: el });
}
