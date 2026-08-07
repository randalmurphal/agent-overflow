export declare class Popover {
    isOpen: boolean;
    content: HTMLDialogElement | undefined;
    reference: HTMLButtonElement | undefined;
    constructor();
    place: (node: HTMLElement) => Promise<void>;
    popoverAttachment: (node: HTMLDialogElement) => () => void;
}
