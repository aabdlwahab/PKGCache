/* Animated GLSL "aurora horizon" background for the top bar.
 *
 * A tiny WebGL renderer for one full-viewport triangle. No dependencies, no build
 * step — same bargain as the rest of the console. Runs under the strict CSP
 * (script-src 'self'): it's a same-origin module and WebGL needs no external
 * resources. Falls back to the topbar's solid --panel background if WebGL is
 * missing, and renders a single static frame under prefers-reduced-motion.
 *
 * The fragment shader is the operator-supplied "aurora" effect; hash/value-noise
 * and fbm helpers are provided here because it depends on them.
 */

const VERT = "attribute vec2 a_pos;void main(){gl_Position=vec4(a_pos,0.0,1.0);}";

const FRAG = `precision highp float;
uniform vec2 u_resolution;
uniform float u_time,u_speed,u_scale,u_softness,u_intensity;
uniform vec3 u_colorA,u_colorB,u_colorC;
float hash(vec2 p){return fract(sin(dot(p,vec2(127.1,311.7)))*43758.5453123);}
float vnoise(vec2 p){
  vec2 i=floor(p),f=fract(p);f=f*f*(3.0-2.0*f);
  float a=hash(i),b=hash(i+vec2(1.0,0.0)),c=hash(i+vec2(0.0,1.0)),d=hash(i+vec2(1.0,1.0));
  return mix(mix(a,b,f.x),mix(c,d,f.x),f.y);
}
float fbm(vec2 p){float v=0.0,a=0.5;for(int i=0;i<5;i++){v+=a*vnoise(p);p*=2.0;a*=0.5;}return v;}
void main(){
  vec2 uv = gl_FragCoord.xy / u_resolution.xy;
  vec2 p = (gl_FragCoord.xy * 2.0 - u_resolution.xy) / u_resolution.y;
  float t = u_time * u_speed;
  float noise = fbm(vec2(p.x * u_scale * 0.55 + t * 0.08, t * 0.04));
  float wave = sin(p.x * u_scale * 1.4 + t * 0.62 + noise * 2.0) * 0.10;
  wave += sin(p.x * u_scale * 0.62 - t * 0.38) * 0.07;
  float horizon = smoothstep(0.34 - u_softness * 0.08, 0.68 + u_softness * 0.08, uv.y + wave);
  float glow = exp(-abs(uv.y + wave - 0.5) * 8.0 / max(0.3, u_softness));
  vec3 color = mix(u_colorA, u_colorB, horizon);
  color = mix(color, u_colorC, glow * 0.62 * u_intensity);
  color += u_colorB * (1.0 - uv.y) * 0.12;
  gl_FragColor = vec4(color, 1.0);
}`;

// Palettes tuned to tokens.css (accent hue 248 = blue) for each theme.
const PALETTES = {
  dark: { A: [0.09, 0.11, 0.15], B: [0.14, 0.24, 0.46], C: [0.32, 0.62, 1.0] },
  light: { A: [0.86, 0.9, 0.97], B: [0.55, 0.7, 0.96], C: [0.35, 0.58, 0.98] },
};

function compile(gl, type, src) {
  const shader = gl.createShader(type);
  gl.shaderSource(shader, src);
  gl.compileShader(shader);
  if (!gl.getShaderParameter(shader, gl.COMPILE_STATUS)) {
    console.warn("topbar shader:", gl.getShaderInfoLog(shader));
    return null;
  }
  return shader;
}

/* How many crests the bar draws, from quiet to saturated. u_scale is the horizontal
 * frequency multiplier for both sine terms in the shader, so this is literally the
 * wave count: one long swell when nothing is happening, a busy chop under load.
 * Speed rides along with it — a fast wave that never multiplies reads as a glitch,
 * and matching the two is what makes the bar legible as a load gauge. */
const SCALE_QUIET = 0.9;
const SCALE_BUSY = 3.6;
const SPEED_QUIET = 0.75;
const SPEED_BUSY = 1.35;
// Seconds for the level to close most of a gap. Traffic arrives in bursts; without
// easing, one `uv sync` snaps the bar from flat to choppy and back like a flicker.
const EASE_TAU = 0.6;

/**
 * @param canvas the topbar canvas
 * @param level optional () => 0..1 traffic reading; omitted means always quiet
 */
export function initTopbarShader(canvas, { level } = {}) {
  const gl = canvas.getContext("webgl") || canvas.getContext("experimental-webgl");
  if (!gl) return; // topbar keeps its solid --panel background

  const vs = compile(gl, gl.VERTEX_SHADER, VERT);
  const fs = compile(gl, gl.FRAGMENT_SHADER, FRAG);
  if (!vs || !fs) return;

  const prog = gl.createProgram();
  gl.attachShader(prog, vs);
  gl.attachShader(prog, fs);
  gl.linkProgram(prog);
  if (!gl.getProgramParameter(prog, gl.LINK_STATUS)) {
    console.warn("topbar shader:", gl.getProgramInfoLog(prog));
    return;
  }
  gl.useProgram(prog);

  const buffer = gl.createBuffer();
  gl.bindBuffer(gl.ARRAY_BUFFER, buffer);
  gl.bufferData(gl.ARRAY_BUFFER, new Float32Array([-1, -1, 3, -1, -1, 3]), gl.STATIC_DRAW);
  const aPos = gl.getAttribLocation(prog, "a_pos");
  gl.enableVertexAttribArray(aPos);
  gl.vertexAttribPointer(aPos, 2, gl.FLOAT, false, 0, 0);

  const U = {};
  for (const name of [
    "u_resolution", "u_time", "u_speed", "u_scale", "u_softness", "u_intensity",
    "u_colorA", "u_colorB", "u_colorC",
  ]) {
    U[name] = gl.getUniformLocation(prog, name);
  }

  function resize() {
    const dpr = Math.min(window.devicePixelRatio || 1, 2);
    const w = Math.max(1, Math.round((canvas.clientWidth || 1) * dpr));
    const h = Math.max(1, Math.round((canvas.clientHeight || 1) * dpr));
    if (canvas.width !== w || canvas.height !== h) {
      canvas.width = w;
      canvas.height = h;
    }
    gl.viewport(0, 0, canvas.width, canvas.height);
  }
  window.addEventListener("resize", resize);
  if (window.ResizeObserver) new ResizeObserver(resize).observe(canvas);

  const reduce =
    window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches;
  const start = performance.now();

  // A level function that throws or returns NaN must not take the topbar's background
  // down with it — this runs 60 times a second, so a bad reading would be 60 errors a
  // second and a dead canvas.
  const read = () => {
    try {
      const value = level ? Number(level()) : 0;
      return Number.isFinite(value) ? Math.min(1, Math.max(0, value)) : 0;
    } catch {
      return 0;
    }
  };

  /* Under prefers-reduced-motion the bar is one static frame, so it samples the level
     once and stays there — following traffic would be exactly the continuous motion
     that preference asks us not to draw. */
  let eased = reduce ? read() : 0;
  let previous = start;

  function frame(now) {
    resize();
    const theme =
      document.documentElement.getAttribute("data-theme") === "light" ? "light" : "dark";
    const pal = PALETTES[theme];

    if (!reduce) {
      // Clamped: a backgrounded tab resumes with a gap of minutes, and an unclamped
      // step would make the bar jump to the new level rather than swell into it.
      const dt = Math.min(0.25, Math.max(0, (now - previous) / 1000));
      previous = now;
      eased += (read() - eased) * (1 - Math.exp(-dt / EASE_TAU));
    }

    gl.uniform2f(U.u_resolution, canvas.width, canvas.height);
    gl.uniform1f(U.u_time, reduce ? 8.0 : (now - start) / 1000);
    gl.uniform1f(U.u_speed, SPEED_QUIET + (SPEED_BUSY - SPEED_QUIET) * eased);
    gl.uniform1f(U.u_scale, SCALE_QUIET + (SCALE_BUSY - SCALE_QUIET) * eased);
    gl.uniform1f(U.u_softness, 1.05);
    gl.uniform1f(U.u_intensity, 0.9);
    gl.uniform3fv(U.u_colorA, pal.A);
    gl.uniform3fv(U.u_colorB, pal.B);
    gl.uniform3fv(U.u_colorC, pal.C);
    gl.drawArrays(gl.TRIANGLES, 0, 3);
    if (!reduce) requestAnimationFrame(frame);
  }
  requestAnimationFrame(frame);
}
