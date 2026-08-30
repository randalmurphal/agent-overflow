import { type StreamdownProps } from './context.svelte.js';
declare function $$render(): {
    props: StreamdownProps;
    exports: {};
    bindings: "streamdown" | "element";
    slots: {};
    events: {};
};
declare class __sveltets_Render {
    props(): ReturnType<typeof $$render>['props'];
    events(): ReturnType<typeof $$render>['events'];
    slots(): ReturnType<typeof $$render>['slots'];
    bindings(): "streamdown" | "element";
    exports(): {};
}
interface $$IsomorphicComponent {
    new (options: import('svelte').ComponentConstructorOptions<ReturnType<__sveltets_Render['props']>>): import('svelte').SvelteComponent<ReturnType<__sveltets_Render['props']>, ReturnType<__sveltets_Render['events']>, ReturnType<__sveltets_Render['slots']>> & {
        $$bindings?: ReturnType<__sveltets_Render['bindings']>;
    } & ReturnType<__sveltets_Render['exports']>;
    (internal: unknown, props: ReturnType<__sveltets_Render['props']> & {}): ReturnType<__sveltets_Render['exports']>;
    z_$$bindings?: ReturnType<__sveltets_Render['bindings']>;
}
declare const Streamdown: $$IsomorphicComponent;
type Streamdown = InstanceType<typeof Streamdown>;
export default Streamdown;
