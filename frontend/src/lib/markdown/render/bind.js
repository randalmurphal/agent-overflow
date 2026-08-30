export const bind = (ref, props) => {
    const descriptors = Object.getOwnPropertyDescriptors(props);
    for (const key in descriptors) {
        Object.defineProperty(ref, key, descriptors[key]);
    }
};
